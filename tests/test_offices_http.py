"""The http transport: milestone 3's region story again, over HTTP.

Each region runs `office serve` on a loopback port, and each peer entry's
address is the other's URL. Placements go to region-a's endpoint as a POST,
replies to region-b's, and region-b's bowery fetches the catalog region-a
kept for it (GET /catalog/region-b). Every test of test_offices_regions runs
here unchanged except for the transport: placement, peer rules, refusals
and their replies, revocation, catalogs, clocks and the missing address.
Then what only HTTP has: a replay over the wire, the limits, and the wake.

    python3 -m unittest discover tests
"""

import json
import re
import subprocess
import time
import unittest
from pathlib import Path
import urllib.error
import urllib.request

import _keys
import test_offices_regions as R
from _binary import OFFICE_BIN
from test_offices import HOST


class OverHTTP:
    def setUp(self):
        self.a_url = self.b_url = "http://127.0.0.1:9"  # nobody, until the endpoints are up
        super().setUp()
        (self.b_root / "etc" / "machine.toml").write_text(f'machine = "{HOST}"\ntransport = "http"\n')
        # An asker's box is there before a reply comes (the install makes it): the endpoint only fills one.
        for var in (self.a_var, self.b_root / "var"):
            for asker in ("bob", "alice"):
                (Path(var) / "boxes" / f"asker-{asker}").mkdir(parents=True, exist_ok=True)
        self.a_url = self.serve(self.env)
        self.b_url = self.serve(self.b_env)
        # The peer entries again, now with the endpoints' URLs.
        self.write_region()
        self.write_b_region()

    def serve(self, env):
        p = subprocess.Popen([OFFICE_BIN, "serve", "--listen", "127.0.0.1:0"], env=env,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

        def stop():
            p.terminate()
            p.communicate(timeout=10)
        self.addCleanup(stop)
        line = p.stdout.readline()
        m = re.match(r"listening on (http://\S+)", line)
        self.assertTrue(m, line + p.stderr.read() if not line else line)
        return m.group(1)

    def write_machine(self, extra=""):
        super().write_machine(extra)
        p = self.etc / "machine.toml"
        p.write_text(p.read_text().replace(f'machine = "{HOST}"\n', f'machine = "{HOST}"\ntransport = "http"\nlisten = "127.0.0.1:0"\n', 1))

    def peer_entry(self, may_place=None, key=None, rules=True):
        return super().peer_entry(may_place, key, rules).replace(f'"{self.b_root / "var"}"', f'"{self.b_url}"')

    def write_b_region(self, key=None):
        super().write_b_region(key)
        p = self.b_root / "etc" / "region.toml"
        p.write_text(p.read_text().replace(f'"{self.a_var}"', f'"{self.a_url}"'))

    def write_b_config(self, extra="", region_key=None):
        super().write_b_config(extra, region_key)
        self.b_config.write_text(self.b_config.read_text().replace("[placement]\n", '[placement]\ntransport = "http"\n', 1))

    def catalog_path(self):
        # Kept by the publisher, for the peer to fetch.
        return self.a_var / "published" / "region-b" / f"{HOST}.catalog"

    def post(self, url, body, ctype="text/plain; charset=utf-8"):
        req = urllib.request.Request(url, data=body.encode() if isinstance(body, str) else body, method="POST")
        if ctype:
            req.add_header("Content-Type", ctype)
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                return r.status
        except urllib.error.HTTPError as e:
            return e.code

    def drop_letter(self, id, body, office=None):
        # Through region-a's endpoint, as a peer's letter arrives.
        box = "office-" + (office or self.REVIEWS)
        self.assertEqual(self.post(f"{self.a_url}/placements/{box}", body), 201)


def over_http(cls):
    return unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")(
        type(cls.__name__ + "OverHTTP", (OverHTTP, cls), {"__doc__": cls.__doc__}))


TheDoneWhenOverHTTP = over_http(R.TheDoneWhen)
CatalogsOverHTTP = over_http(R.Catalogs)
ClocksOverHTTP = over_http(R.Clocks)


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class SignaturesOverHTTP(OverHTTP, R.Signatures):
    def test_a_principal_can_not_pass_as_this_regions_by_leaving_out_from(self):
        self.drop_letter("own-bob", self.peer_letter("own-bob", seal=None, **{"from": "region-a"}))
        self.assertEqual(self.pull()["dropped"][0]["reason"], "it isn't signed by a listed signer")
        # A qualified asker doesn't decode, so the endpoint never stores it.
        box = f"{self.a_url}/placements/office-{self.REVIEWS}"
        self.assertEqual(self.post(box, self.peer_letter("q-1", asked_by="bob@region-b", **{"from": "region-a"})), 400)
        self.assertEqual(list((self.a_var / "boxes").rglob("q-1*")), [])


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class APeerWithNoAddressOverHTTP(OverHTTP, R.APeerWithNoAddress):
    def peer_entry(self, may_place=None, key=None, rules=True):
        return re.sub(r'address   = "[^"]*"', 'address   = ""', super().peer_entry(may_place, key, rules))

    def write_region(self, extra=None):
        if not hasattr(self, "b_root"):
            return R.Regions.write_region(self, "")
        R.Regions.write_region(self, self.peer_entry())


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class OnlyOverHTTP(OverHTTP, R.Regions):
    def test_the_peer_addresses_are_urls(self):
        self.assertIn(f'address   = "{self.b_url}"', (self.etc / "region.toml").read_text())
        self.assertIn(f'address   = "{self.a_url}"', (self.b_root / "etc" / "region.toml").read_text())
        self.office_ok("check", self.REVIEWS)

    def test_a_replay_over_the_wire_starts_nothing_and_is_answered_once(self):
        self.publish()
        self.b_place("--kind", "review", "--id", "rp-1", code=0)
        self.pull()
        self.assertEqual(self.started_ids(), ["rp-1"])
        letter = (self.a_var / self.REVIEWS / "accepted" / "rp-1.placement").read_text()
        for _ in range(2):
            self.drop_letter("rp-1", letter)
            self.pull()
        self.assertEqual(self.started_ids(), ["rp-1"])
        self.assertIn("already accepted rp-1", self.b_replies()["rp-1"])
        dups = self.events(self.REVIEWS, "place-duplicate")
        self.assertEqual([d["replied"] for d in dups], [True, False])

    def test_a_refusal_reaches_the_asker_through_its_regions_endpoint(self):
        self.drop_letter("rf-1", self.peer_letter("rf-1", seal="region-x"))
        self.pull()
        self.assertIn("refused rf-1", self.b_replies()["rf-1"])
        # Region-b's endpoint down: the reply can't be made, and that is logged.
        self.b_url = "http://127.0.0.1:9"
        self.write_region()
        self.drop_letter("rf-2", self.peer_letter("rf-2", seal="region-x"))
        self.pull()
        failed = self.events(self.REVIEWS, "reply-failed")
        self.assertEqual(failed[-1]["job"], "rf-2")
        self.assertNotIn("rf-2", self.b_replies())

    def test_the_endpoint_refuses_oversize_traversal_and_strangers(self):
        box = f"{self.a_url}/placements/office-{self.REVIEWS}"
        big = self.peer_letter("big-1") + "x" * (64 << 10)
        self.assertEqual(self.post(box, big), 413)
        self.assertEqual(self.post(box, self.peer_letter("ct-1"), ctype="application/json"), 415)
        self.assertEqual(self.post(box, "not a letter\n"), 400)
        for path in ("/placements/..", "/placements/%2E%2E%2Fowner", f"/placements/office-{self.REVIEWS}%2F..",
                     "/placements/office-region-a-faraway", "/replies/..%2F..", "/replies/a%2Fb?id=x",
                     "/owner/x", "/"):
            self.assertEqual(self.post(self.a_url + path + ("" if "?" in path else "?id=x"), self.peer_letter("tr-1")), 404, path)
        for path in ("/catalog/..", "/catalog/%2E%2E%2Fboxes", "/catalog/region-c", f"/placements/office-{self.REVIEWS}"):
            try:
                with urllib.request.urlopen(self.a_url + path, timeout=10) as r:
                    code = r.status
            except urllib.error.HTTPError as e:
                code = e.code
            self.assertIn(code, (404, 405), path)
        self.assertEqual(self.pull()["accepted"], [])
        self.assertEqual(sorted(p.name for p in (self.a_var / "boxes").rglob("*") if p.is_file()), [])

    def test_a_post_wakes_the_pull_loop_within_50ms(self):
        self.publish()
        loop = subprocess.Popen([OFFICE_BIN, "pull", self.REVIEWS, "--loop", "--interval", "1h", "--backstop", "1h",
                                 "--for", "30s"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            gaps = []
            for i in range(5):
                time.sleep(0.4)  # the loop is back in its wait
                self.b_place("--kind", "review", "--id", f"wk-{i}", code=0)
                deadline = time.monotonic() + 10
                launched = []
                # The agent starts before the office logs its launch: wait
                # for the log line, not the start.
                while time.monotonic() < deadline and not launched:
                    launched = [e for e in self.events(self.REVIEWS, "launched") if e["job"] == f"wk-{i}"]
                    time.sleep(0.02)
                self.assertTrue(launched, f"wk-{i} was never launched")
                b = self.b_place_log()[-1]["stages"]
                a = launched[0]["stages"]
                gaps.append((stamp(a["delivered"]) - stamp(b["send"]), stamp(a["accepted"]) - stamp(b["send"])))
                # Ended, so the next pass has no run phase to judge before
                # it reads the inbox: what is measured is the hop and the wake.
                self.office_ok("done", self.REVIEWS, f"wk-{i}")
        finally:
            loop.terminate()
            loop.communicate(timeout=10)
        for delivered, accepted in gaps:
            # From bowery's send to the office reading it: the POST, the
            # store, the watch's wake and the pass's read.
            self.assertLess(delivered, 0.05, gaps)
            # Accept adds the rules (here two Python plugins starting).
            self.assertLess(accepted, 1.0, gaps)
        print(f"\n  http hop, send to delivered / accepted (ms): "
              + ", ".join(f"{d * 1000:.0f}/{a * 1000:.0f}" for d, a in gaps), end=" ")


def stamp(s):
    from datetime import datetime
    return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


if __name__ == "__main__":
    unittest.main()
