"""Offices, milestone 3: regions and peering, in dev mode.

Two regions on one machine, each its own OFFICE_ROOT: region-a holds the
offices, region-b has only a dispatcher (bob's bowery). Each declares the
other with the other's pinned key; region-a lets region-b place into its
reviews office alone, under a peer rule. Placements are signed by the asker
and, between regions, sealed by the asker's region (ssh-keygen -Y
signatures); catalogs are signed by the publishing region. Everything moves
through the transport interface (the dir transport, whose address is the
peer region's var directory).

The plan's done-when: a peer places only within its peer rules; an unpinned
key is refused; removing the peer entry refuses the next placement. And the
spec's: a bowery never sees an office a peer didn't publish to it.

    python3 -m unittest discover tests
"""

import json
import os
import subprocess
import sys
import textwrap
import time
import unittest
from pathlib import Path

import _keys
from _binary import BIN, OFFICE_BIN, SLOW_CI
from test_offices import HOST, Offices

SURFACE = f"""#!{sys.executable}
import json, os, sys
job = json.load(sys.stdin)
action = sys.argv[1] if len(sys.argv) > 1 else "start"
here = os.path.dirname(os.path.abspath(sys.argv[0]))
with open(os.path.join(here, "surface.log"), "a") as f:
    f.write(" ".join([job["id"], action, os.environ.get("OFFICE_ON_CLOSED", "-")]) + "\\n")
if action == "start":
    with open(os.path.join(here, "started"), "a") as f:
        f.write(job["id"] + " " + os.environ["OFFICE_NAME"] + "\\n")
"""

STAGES_B = ("resolve", "resolved", "sign", "seal", "send", "sent")
STAGES_A = ("delivered", "verified", "accepted", "started")


class Regions(Offices):
    def setUp(self):
        super().setUp()
        self.surface.write_text(SURFACE)
        self.peer_flag = self.t / "peer-flag.json"
        self.set_peer_flag("open")
        self.a_var = self.var
        self.b_root = self.t / "region-b"
        (self.b_root / "etc").mkdir(parents=True)
        self.b_state = self.t / "b-state"
        self.b_config = self.t / "b-config.toml"
        self.b_env = {**self.env, "OFFICE_ROOT": str(self.b_root), "BOWERY_STATE": str(self.b_state),
                      "BOWERY_CONFIG": str(self.b_config), "USER": "bob"}
        self.write_b_config()
        self.write_b_region()
        self.write_machine()
        self.write_region()
        self.write_signers()

    # ------------------------------------------------------------ region-a

    def write_machine(self, extra=""):
        super().write_machine(extra)
        p = self.etc / "machine.toml"
        p.write_text(p.read_text().replace(
            f'machine = "{HOST}"\n', f'machine = "{HOST}"\nregion_key = "{_keys.key("region-a")}"\n', 1).replace(
            f'command = ["{self.surface}"]',
            f'command = ["{self.surface}", "start"]\nhold = ["{self.surface}", "hold"]\n'
            f'resume = ["{self.surface}", "resume"]\nstop = ["{self.surface}", "stop"]'))

    def peer_entry(self, may_place=None, key=None, rules=True):
        may = may_place if may_place is not None else [self.REVIEWS]
        s = textwrap.dedent(f"""
            [peers.region-b]
            address   = "{self.b_root / 'var'}"
            key       = "{key or _keys.pub('region-b')}"
            may_place = {json.dumps(may)}
            may_view  = []
            """)
        if rules:
            s += textwrap.dedent(f"""
                [[peers.region-b.rules]]
                name   = "peer-gate"
                plugin = "flag"
                [peers.region-b.rules.config]
                file   = "{self.peer_flag}"
                """)
        return s

    def write_region(self, extra=None):
        if extra is None:
            extra = self.peer_entry() if hasattr(self, "b_root") else ""
        super().write_region(extra)

    def write_signers(self, office=None):
        # region-b's bob may sign here, under his own name and region.
        d = self.etc / (office or self.REVIEWS)
        (d / "signers").write_text(f"alice {_keys.pub('alice')} alice\n"
                                   f"bob@region-b {_keys.pub('bob')} bob of region-b\n")

    def set_peer_flag(self, verdict, **extra):
        v = {"verdict": verdict, **extra}
        if verdict == "closed" and "reason" not in v:
            v["reason"] = "region-b's work waits for the evening"
        self.peer_flag.write_text(json.dumps(v))

    def publish(self):
        return json.loads(self.office_ok("publish", "--json"))

    # ------------------------------------------------------------ region-b

    def write_b_region(self, key=None):
        (self.b_root / "etc" / "region.toml").write_text(textwrap.dedent(f"""\
            region   = "region-b"
            owner    = "bob"
            timezone = "UTC"
            key      = "{_keys.pub('region-b')}"
            offices  = []
            priorities  = ["high", "low"]
            max_expires = "24h"

            [peers.region-a]
            address   = "{self.a_var}"
            key       = "{key or _keys.pub('region-a')}"
            may_place = []
            may_view  = []
            """))

    def write_b_config(self, extra="", region_key=None):
        self.b_config.write_text(textwrap.dedent(f"""\
            [placement]
            office_command = "{OFFICE_BIN}"
            keys           = {{ bob = "{_keys.key('bob')}" }}
            region_key     = "{region_key or _keys.key('region-b')}"
            consider.regions = ["region-b", "region-a"]
            """) + extra)

    def b_bowery(self, *args):
        return subprocess.run([BIN, *args], env=self.b_env, capture_output=True, text=True, timeout=60)

    def b_place(self, *args, code=None):
        r = self.b_bowery("place", *args)
        if code is not None:
            self.assertEqual(r.returncode, code, r.stdout + r.stderr)
        return json.loads(r.stdout) if r.stdout.strip() else {}, r

    def b_place_log(self):
        p = self.b_state / "place.log"
        return [json.loads(l) for l in p.read_text().splitlines()] if p.exists() else []

    def catalog_path(self):
        """Where region-a's catalog for region-b is kept: pushed into
        region-b's var by the dir transport."""
        return self.b_root / "var" / "catalogs" / "region-a" / f"{HOST}.catalog"

    def b_replies(self, who="bob"):
        d = self.b_root / "var" / "boxes" / f"asker-{who}"
        return {p.stem: p.read_text() for p in d.glob("*.reply")} if d.exists() else {}

    def peer_letter(self, id, signer="bob", seal="region-b", office=None, **kw):
        from datetime import datetime, timedelta, timezone
        now = datetime.now(timezone.utc)
        fields = {"id": id, "kind": "review", "asked_by": "bob", "from": "region-b",
                  "expires": iso(now + timedelta(hours=1)), "signed_at": iso(now),
                  "office": office or self.REVIEWS, "machine": HOST, "region": "region-a"}
        fields.update(kw)
        block = "```placement\n" + "".join(f"{k} = {json.dumps(v)}\n" for k, v in sorted(fields.items())) + "```\n"
        return _keys.letter(block, requester=signer, region=seal)

    def drop_letter(self, id, body, office=None):
        box = self.a_var / "boxes" / ("office-" + (office or self.REVIEWS))
        box.mkdir(parents=True, exist_ok=True)
        (box / f"{id}.placement").write_text(body)


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class TheDoneWhen(Regions):
    def test_a_peer_places_end_to_end_with_every_stage_timed_across_the_hop(self):
        rep = self.publish()
        self.assertEqual(rep, [{"peer": "region-b", "offices": [self.REVIEWS]}])
        loop = subprocess.Popen([OFFICE_BIN, "pull", self.REVIEWS, "--loop", "--interval", "200ms", "--for", "20s"],
                                env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            time.sleep(0.5)
            t0 = time.monotonic()
            out, _ = self.b_place("--kind", "review", "--id", "peer-1", "--", "review PR 7", code=0)
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline and "peer-1" not in self.started_ids():
                time.sleep(0.05)
            took = time.monotonic() - t0
        finally:
            loop.terminate()
            loop.communicate(timeout=10)
        self.assertEqual(self.started_ids(), ["peer-1"])
        if not SLOW_CI:  # timing only; see SLOW_CI in _binary.py (CI-FAST)
            self.assertLess(took, 5, "placement to started across regions")
        placed = out["placed"]
        self.assertEqual((placed["office"], placed["region"]), (self.REVIEWS, "region-a"))
        self.assertEqual(set(placed["stages"]), set(STAGES_B))

        # The office knows who signed and which region sealed it.
        delivered = self.events(self.REVIEWS, "delivered")[0]
        self.assertEqual(delivered["from"], "region-b")
        self.assertTrue(delivered["seal"].startswith("SHA256:"))
        accepted = self.events(self.REVIEWS, "place-accepted")[0]
        self.assertEqual((accepted["from"], accepted["asked_by"]), ("region-b", "bob"))

        # One timeline across the hop: bowery's place.log in region-b, then
        # the office log in region-a, each stage no earlier than the last.
        b = self.b_place_log()[-1]["stages"]
        a = self.events(self.REVIEWS, "launched")[0]["stages"]
        self.assertEqual(a["resolve"], b["resolve"])
        self.assertEqual(a["sign"], b["sign"])
        # "sent" is bowery's stamp after the send returns, and the office may
        # take the letter in before that (seen on macOS: delivered 9ms ahead
        # of sent), so it is ordered after "send" only, not before
        # "delivered".
        self.assertLessEqual(b["send"], b["sent"])
        timeline = [b[k] for k in STAGES_B if k != "sent"] + [a[k] for k in STAGES_A]
        self.assertEqual(timeline, sorted(timeline))

        # The peer rule judged it, as a peer's.
        peer_verdicts = [e for e in self.events(self.REVIEWS, "verdict") if e["instance"] == "peer-gate"]
        self.assertTrue(peer_verdicts)
        self.assertEqual(peer_verdicts[0]["source"], "peer")

    def test_a_peer_places_only_within_its_peer_rules(self):
        self.publish()
        self.set_peer_flag("closed")
        self.b_place("--kind", "review", "--id", "peer-2", code=0)  # the catalog is advisory
        rep = self.pull()
        self.assertEqual(rep["waiting"], [{"id": "peer-2", "reason": "region-b's work waits for the evening"}])
        self.assertEqual(self.started_ids(), [])
        # The same office takes its own region's work: the peer rule is the
        # peer's alone.
        self.place("--kind", "review", "--id", "own-1", "--pin", self.REVIEWS, code=0)
        self.pull()
        self.assertEqual(self.started_ids(), ["own-1"])
        own = [e for e in self.events(self.REVIEWS, "verdict") if e["job"] == "own-1"]
        self.assertNotIn("peer-gate", [e["instance"] for e in own])
        self.set_peer_flag("open")
        self.pull()
        self.assertEqual(self.started_ids(), ["own-1", "peer-2"])

        # may_place is the other half: a peer's letter to an office it may
        # not place into is refused, and its asker is told.
        self.drop_letter("peer-3", self.peer_letter("peer-3", office=self.BUILDER, kind="builder"), office=self.BUILDER)
        self.write_signers(self.BUILDER)
        rep = self.pull(self.BUILDER)
        self.assertEqual(rep["refused"], [{"id": "peer-3", "reason": f"region region-b may not place into {self.BUILDER}"}])
        self.assertIn("may not place", self.b_replies()["peer-3"])

    def test_an_unpinned_key_is_refused(self):
        # A placement sealed by a key region-a doesn't pin for region-b.
        self.drop_letter("seal-1", self.peer_letter("seal-1", seal="region-x"))
        rep = self.pull()
        self.assertEqual(len(rep["refused"]), 1)
        self.assertIn("not by the key region region-a pins", rep["refused"][0]["reason"])
        self.assertIn("refused seal-1", self.b_replies()["seal-1"])
        # No seal at all.
        self.drop_letter("seal-2", self.peer_letter("seal-2", seal=None))
        self.assertIn("carries no region seal", self.pull()["refused"][0]["reason"])
        # A requester key the signers file doesn't list: dropped, unanswered.
        self.drop_letter("req-1", self.peer_letter("req-1", signer="mallory"))
        rep = self.pull()
        self.assertEqual(rep["dropped"], [{"id": "req-1", "reason": "it isn't signed by a listed signer"}])
        self.assertNotIn("req-1", self.b_replies())
        self.assertEqual(self.started_ids(), [])

        # And a catalog region-b's pin for region-a doesn't match: no link,
        # so no office of region-a's is ever planned over.
        self.publish()
        self.write_b_region(key=_keys.pub("region-x"))
        out, r = self.b_place("--kind", "review", "--id", "nolink-1", code=1)
        self.assertIn("unplaceable", out)
        rows = {c["office"]: c for c in out["unplaceable"]["candidates"]}
        self.assertEqual(rows["*"]["class"], "excluded")
        self.assertIn("not by the key pinned for it: no link", rows["*"]["reason"])
        self.assertNotIn(self.REVIEWS, rows)
        peers = json.loads(self.b_bowery("peers", "--json").stdout)
        self.assertEqual(peers[0]["offices"], [])
        self.assertIn("no link", peers[0]["problems"][0])

    def test_removing_the_peer_entry_refuses_the_next_placement_and_holds_running_work(self):
        self.publish()
        self.b_place("--kind", "review", "--id", "rv-1", code=0)
        self.pull()
        self.assertEqual(self.started_ids(), ["rv-1"])

        self.write_region("")  # region-a no longer declares region-b
        self.b_place("--kind", "review", "--id", "rv-2", code=0)  # region-b's catalog is still cached
        rep = self.pull()
        self.assertEqual(rep["refused"], [{"id": "rv-2", "reason": "region region-b isn't a peer of region region-a"}])
        self.assertEqual(self.started_ids(), ["rv-1"])
        failed = self.events(self.REVIEWS, "reply-failed")
        self.assertIn("no address", failed[0]["why"])
        # The running job is asked per the office's on_closed, and stays.
        self.assertEqual(rep["run"][0]["id"], "rv-1")
        self.assertIn("region region-b is no longer a peer", rep["run"][0]["reason"])
        calls = (self.lib / "surface.log").read_text().split("\n")
        self.assertIn("rv-1 hold yield", calls)
        # The owner's open doesn't bring a revoked link back.
        self.office_ok("open", self.REVIEWS)
        self.pull()
        held = json.loads(self.office_ok("describe", self.REVIEWS, "--json"))["jobs"][0]
        self.assertEqual((held["id"], held["state"]), ("rv-1", "held"))
        # Declared again: it resumes in place.
        self.write_region()
        self.office_ok("clear", self.REVIEWS)
        self.pull()
        self.assertIn("rv-1 resume -", (self.lib / "surface.log").read_text())

    def test_narrowing_may_place_reaches_the_peer_on_the_next_publish(self):
        self.publish()
        self.write_region(self.peer_entry(may_place=[]))
        self.assertEqual(self.publish(), [{"peer": "region-b", "offices": []}])
        out, _ = self.b_place("--kind", "review", "--id", "nar-1", code=1)
        self.assertNotIn(self.REVIEWS, [c["office"] for c in out["unplaceable"]["candidates"]])


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class Catalogs(Regions):
    def test_a_bowery_never_sees_an_office_a_peer_did_not_publish_to_it(self):
        self.publish()
        peers = json.loads(self.b_bowery("peers", "--json").stdout)
        self.assertEqual(peers[0]["offices"], [self.REVIEWS])  # not the builder
        out, _ = self.b_place("--kind", "builder", "--id", "cat-1", code=1)
        self.assertNotIn(self.BUILDER, [c["office"] for c in out["unplaceable"]["candidates"]])
        out, _ = self.b_place("--kind", "builder", "--id", "cat-2", "--pin", self.BUILDER, code=1)
        pinned = [c for c in out["unplaceable"]["candidates"] if c["office"] == self.BUILDER][0]
        self.assertEqual(pinned["reason"], "not an office this bowery knows")
        # What a peer sees carries nothing about other jobs, nor local paths.
        body = self.catalog_path().read_text()
        doc = json.loads(body.split("\n")[1])
        self.assertEqual(doc["for"], "region-b")
        self.assertEqual(doc["offices"][0]["box_path"], "")
        self.assertEqual(doc["offices"][0]["jobs"], [])

    def test_a_tampered_catalog_is_no_link(self):
        self.publish()
        p = self.catalog_path()
        p.write_text(p.read_text().replace('"kinds":["review"]', '"kinds":["review","builder"]'))
        peers = json.loads(self.b_bowery("peers", "--json").stdout)
        self.assertEqual(peers[0]["offices"], [])
        self.assertIn("doesn't verify", peers[0]["problems"][0])

    def test_a_missing_or_stale_catalog_is_a_wait_not_a_setup_gap(self):
        out, _ = self.b_place("--kind", "review", "--id", "st-1", code=4)
        self.assertIn("queued", out)
        self.office_ok("publish", "--valid", "1s")
        time.sleep(1.2)
        _, r = self.b_place("--kind", "review", "--id", "st-2", code=4)
        self.assertIn("stale", r.stdout)

    def test_the_peer_filter_uses_the_catalog_state_and_vocabulary(self):
        self.office_ok("close", self.REVIEWS)
        self.publish()
        out, _ = self.b_place("--kind", "review", "--id", "pf-1", code=4)
        self.assertIn("closed by its owner", out["queued"]["waiting_for"])
        self.office_ok("clear", self.REVIEWS)
        self.publish()
        _, r = self.b_place("--kind", "review", "--id", "pf-2", "--priority", "low", code=0)
        # A peer office's reserved effects are its region's.
        self.write_region(self.peer_entry() + "")
        _, r = self.b_place("--kind", "review", "--id", "pf-3", "--effects", "spend", code=1)
        self.assertIn("effect spend is reserved", r.stdout)

    def test_a_seal_needs_this_regions_own_key(self):
        self.publish()
        self.write_b_config(region_key=_keys.key("region-x"))
        _, r = self.b_place("--kind", "review", "--id", "rk-1", code=1)
        self.assertIn("isn't region region-b's key", r.stderr)
        self.assertEqual(list((self.a_var / "boxes").glob("*/rk-1.placement")), [])


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class Signatures(Regions):
    def test_a_principal_can_not_pass_as_this_regions_by_leaving_out_from(self):
        # bob's key, written as region-a's own bob: no signers line says so.
        self.drop_letter("own-bob", self.peer_letter("own-bob", seal=None, **{"from": "region-a"}))
        self.assertEqual(self.pull()["dropped"][0]["reason"], "it isn't signed by a listed signer")
        # A qualified asker in a block is refused outright.
        self.drop_letter("q-1", self.peer_letter("q-1", asked_by="bob@region-b", **{"from": "region-a"}))
        self.assertEqual(self.pull()["dropped"][0]["reason"], "not a placement this office can read")

    def test_a_placement_with_no_key_for_its_asker_is_not_placed(self):
        self.write_b_config()
        _, r = self.b_place("--kind", "review", "--asked-by", "carol", code=1)
        self.assertIn("no signing key for carol", r.stderr)

    def test_signers_options_and_check(self):
        (self.etc / self.REVIEWS / "signers").write_text(
            f'alice namespaces="placement" {_keys.pub("alice")}\n'
            f'manager namespaces="office-state" {_keys.pub("manager")}\n'
            'old ssh-ed25519 AAAA an M1 line\n')
        own = {"from": "region-a", "seal": None}
        self.drop_letter("ns-1", self.peer_letter("ns-1", signer="alice", asked_by="alice", **own))
        self.drop_letter("ns-2", self.peer_letter("ns-2", signer="manager", asked_by="manager", **own))
        rep = self.pull()
        self.assertEqual([a["id"] for a in rep["accepted"]], ["ns-1"])
        self.assertEqual(rep["dropped"], [{"id": "ns-2", "reason": "it isn't signed by a listed signer"}])
        r = self.office("check", self.REVIEWS)
        self.assertEqual(r.returncode, 1)
        self.assertIn("signer old:", r.stderr)

    def test_check_reports_a_peer_with_an_unusable_pin(self):
        self.write_region(self.peer_entry(key="ssh-ed25519 AAAA… region-b"))
        r = self.office("check", self.REVIEWS)
        self.assertEqual(r.returncode, 1)
        self.assertIn("peer region-b: its pinned key isn't usable", r.stderr)
        self.assertIn("ok", self.office_ok("check", self.BUILDER))  # region-b may not place there

    def test_a_letter_is_checkable_with_ssh_keygen(self):
        self.publish()
        self.b_place("--kind", "review", "--id", "hand-1", code=0)
        body = (self.a_var / "boxes" / f"office-{self.REVIEWS}" / "hand-1.placement").read_text()
        block, rest = body.split("```signature requester\n", 1)
        sig = rest.split("\n```\n", 1)[0] + "\n"
        (self.t / "sig").write_text(sig)
        r = subprocess.run(["ssh-keygen", "-Y", "verify", "-f", str(self.etc / self.REVIEWS / "signers"),
                            "-I", "bob@region-b", "-n", "placement", "-s", str(self.t / "sig")],
                           input=block, capture_output=True, text=True)
        self.assertEqual(r.returncode, 0, r.stderr)


def iso(dt):
    return dt.strftime("%Y-%m-%dT%H:%M:%SZ")


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class Clocks(Regions):
    """The peer hop keeps M2's clock rules: another region's time dated in
    the future is refused, and a retry time already past never spins."""

    def ahead(self, **kw):
        from datetime import datetime, timedelta, timezone
        return iso(datetime.now(timezone.utc) + timedelta(**kw))

    def test_a_catalog_published_in_the_future_is_refused(self):
        self.office_ok("publish", env={"OFFICE_NOW": self.ahead(hours=1)})
        out, r = self.b_place("--kind", "review", "--id", "fut-1", code=4)  # a wait: the peer may fix its clock
        self.assertIn("ahead of this clock", out["queued"]["waiting_for"])
        self.assertEqual(list((self.a_var / "boxes").glob("*/fut-1.placement")), [])
        self.publish()  # its clock right again
        self.b_place("--kind", "review", "--id", "fut-2", code=0)

    def test_an_owners_state_dated_in_the_future_is_refused(self):
        self.office_ok("open", self.REVIEWS, env={"OFFICE_NOW": self.ahead(hours=1)})
        self.publish()
        out, _ = self.b_place("--kind", "review", "--id", "fst-1", code=4)
        rows = {c["office"]: c for c in json.loads(self.b_bowery("queue", "--json").stdout)[0]["filter"]}
        self.assertEqual(rows[self.REVIEWS]["class"], "unreachable")
        self.assertIn("dated ahead of this clock", rows[self.REVIEWS]["reason"])

    def test_a_peer_placement_signed_in_the_future_is_refused(self):
        self.drop_letter("fsig-1", self.peer_letter("fsig-1", signed_at=self.ahead(hours=1)))
        rep = self.pull()
        self.assertEqual(len(rep["refused"]), 1)
        self.assertIn("ahead of this clock", rep["refused"][0]["reason"])
        self.drop_letter("fsig-2", self.peer_letter("fsig-2", signed_at=self.ahead(seconds=-5), expires=self.ahead(hours=1)))
        self.assertEqual([a["id"] for a in self.pull()["accepted"]], ["fsig-2"])

    def test_a_past_retry_from_a_peer_never_spins(self):
        past = self.ahead(hours=-1)
        # The resolver: a catalog's closed verdict with a retry already past
        # isn't booked as the job's retry.
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.write_signers()
        self.set_flag("closed", reason="lagging", retry_after=past, holds="office", valid_until=self.ahead(hours=1))
        self.probe(self.REVIEWS, self.job())  # publishes the office-wide verdict
        self.publish()
        out, _ = self.b_place("--kind", "review", "--id", "rp-1", code=4)
        self.assertEqual(out["queued"]["retry_after"], "")
        retry = json.loads(self.b_bowery("place", "--retry").stdout)
        self.assertNotIn("next_retry", retry)
        # The office: a peer rule closed with a retry already past, under the
        # long-poll, is judged once and on events, not every few ms.
        self.set_flag("open")
        self.set_peer_flag("closed", retry_after=past)
        self.drop_letter("rp-2", self.peer_letter("rp-2"))
        loop = subprocess.run([OFFICE_BIN, "pull", self.REVIEWS, "--loop", "--interval", "100ms", "--backstop", "60s",
                               "--for", "3s"], env=self.env, capture_output=True, text=True, timeout=20)
        passes = [e for e in self.log(self.REVIEWS) if e["event"] == "evaluated" and e.get("job") == "rp-2"]
        self.assertGreaterEqual(len(passes), 1)
        self.assertLess(len(passes), 5, loop.stderr[-500:])
        self.assertEqual(self.started_ids(), [])


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class APeerWithNoAddress(Regions):
    """An empty address means "this region's own" to the transport, so a peer
    entry without one must never be sent to, or replied to: its asker's
    refusal would land in a local principal's box, and its offices' placements
    in this region's own boxes."""

    def write_region(self, extra=None):
        if not hasattr(self, "b_root"):
            return super().write_region("")
        super().write_region(self.peer_entry().replace(f'address   = "{self.b_root / "var"}"', 'address   = ""'))

    def test_a_refusal_is_not_filed_in_a_local_askers_box(self):
        self.drop_letter("na-1", self.peer_letter("na-1", office=self.BUILDER, kind="builder"), office=self.BUILDER)
        self.write_signers(self.BUILDER)
        rep = self.pull(self.BUILDER)
        self.assertEqual([r["id"] for r in rep["refused"]], ["na-1"])
        self.assertEqual(list((self.a_var / "boxes").glob("asker-*/*.reply")), [])
        failed = self.events(self.BUILDER, "reply-failed")
        self.assertIn("no address", failed[0]["why"])

    def test_a_catalog_is_not_published_to_it(self):
        r = self.office("publish", "--json")
        self.assertEqual(r.returncode, 1, r.stderr)
        self.assertIn("no address", json.loads(r.stdout)[0]["error"])
        self.assertFalse((self.a_var / "catalogs").exists())


if __name__ == "__main__":
    unittest.main()
