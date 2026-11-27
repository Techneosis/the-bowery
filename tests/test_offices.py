"""Offices in dev mode: the rule contract and evaluator, the hours plugin,
office open/close/describe/log/pull/check, and bowery place, its queue,
place.log and dispatch's job flags.

Every test gets its own OFFICE_ROOT (dev mode: config, state, inboxes and
logs under one directory, run as you) with a region, a machine file naming
this host, and two offices. Rule plugins are the real hours binary and a
small Python plugin whose behaviour its config picks (answer from a file,
crash, hang, talk nonsense), pinned by sha256 like any other.

    python3 -m unittest discover tests
"""

import hashlib
import json
import os
import pwd
import socket
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest
from pathlib import Path

import _keys
from _binary import BIN, HOURS_BIN, OFFICE_BIN

HOST = socket.gethostname().split(".")[0]

PLUGIN = f"""#!{sys.executable}
import json, sys, time
req = json.load(sys.stdin)
if req.get("describe"):
    print(json.dumps({{"name": "flag", "version": "0.1.0", "contracts": [1], "phases": ["place", "accept", "run"],
        "facts": [], "history": "1h", "config_schema": {{"type": "object", "additionalProperties": False,
        "properties": {{"file": {{"type": "string"}}, "behave": {{"enum": ["file", "crash", "hang", "nonsense", "noreason", "extra"]}},
                       "seen": {{"type": "string"}}}}}}}}))
    sys.exit(0)
cfg = req["rule"]["config"]
if cfg.get("seen"):
    with open(cfg["seen"], "a") as f:
        f.write(json.dumps(req) + "\\n")
b = cfg.get("behave", "file")
if b == "crash":
    sys.exit(3)
if b == "hang":
    time.sleep(30)
if b == "nonsense":
    print("this is not json")
elif b == "noreason":
    print(json.dumps({{"verdict": "closed"}}))
elif b == "extra":
    print(json.dumps({{"verdict": "open", "surprise": 1}}))
else:
    print(open(cfg["file"]).read())
"""

SURFACE = f"""#!{sys.executable}
import json, os, sys
job = json.load(sys.stdin)
with open(os.path.join(os.path.dirname(os.path.abspath(sys.argv[0])), "started"), "a") as f:
    f.write(job["id"] + " " + os.environ["OFFICE_NAME"] + "\\n")
"""


def sha(p):
    return hashlib.sha256(Path(p).read_bytes()).hexdigest()


class Offices(unittest.TestCase):
    REVIEWS = "region-a-reviews"
    BUILDER = "region-a-builder"

    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="offices-")
        self.t = t = Path(os.path.realpath(self._tmp.name))
        self.root = t / "offices"
        self.etc = self.root / "etc"
        self.var = self.root / "var"
        self.lib = t / "lib"
        for d in (self.etc / self.REVIEWS, self.etc / self.BUILDER, self.lib, t / "home"):
            d.mkdir(parents=True)
        self.plugin = self.lib / "flag"
        self.plugin.write_text(PLUGIN)
        self.plugin.chmod(0o755)
        self.surface = self.lib / "surface"
        self.surface.write_text(SURFACE)
        self.surface.chmod(0o755)
        self.started = self.lib / "started"
        self.flag = t / "flag.json"
        self.set_flag("open")
        self.config = t / "config.toml"
        self.write_config()
        self.state = t / "state"
        self.env = {
            "PATH": os.environ["PATH"],
            "HOME": str(t / "home"),
            "OFFICE_ROOT": str(self.root),
            "BOWERY_STATE": str(self.state),
            "BOWERY_CONFIG": str(self.config),
            "USER": "alice",
        }
        self.write_region()
        self.write_machine()
        self.write_office(self.REVIEWS, kinds=["review"], tools=["git"], rules="")
        self.write_office(self.BUILDER, kinds=["builder"], tools=["git", "go"], rules="")

    def tearDown(self):
        # Cleanups (a node or loop a test started) run after tearDown, so
        # run them first: a process still writing would refill the folder
        # as it is removed.
        self.doCleanups()
        self._tmp.cleanup()

    # ------------------------------------------------------------ config

    PRINCIPALS = ("alice", "manager", "mallory")

    def write_config(self, extra="", office_command=None):
        """bowery's [placement]: the office host, and every principal's
        signing key (a placement nobody signed is dropped)."""
        keys = ", ".join(f'{p} = "{_keys.key(p)}"' for p in self.PRINCIPALS)
        self.config.write_text(f'[placement]\noffice_command = "{office_command or OFFICE_BIN}"\n'
                               f'keys = {{ {keys} }}\n' + extra)

    def set_flag(self, verdict, **extra):
        v = {"verdict": verdict, **extra}
        if verdict == "closed" and "reason" not in v:
            v["reason"] = "the flag says closed"
        self.flag.write_text(json.dumps(v))

    def write_region(self, extra=""):
        (self.etc / "region.toml").write_text(textwrap.dedent(f"""\
            region   = "region-a"
            owner    = "alice"
            timezone = "UTC"
            key      = "{_keys.pub('region-a')}"
            offices  = [{{ name = "{self.REVIEWS}", machine = "{HOST}" }},
                        {{ name = "{self.BUILDER}", machine = "{HOST}" }},
                        {{ name = "region-a-faraway", machine = "box-1" }}]
            priorities       = ["high", "low"]
            reserved_effects = ["spend"]
            max_expires      = "24h"

            [tiers.reader]
            effects = ["push"]
            holds   = ["tools", "repos"]

            [naming]
            user_max = 32
            box      = "office-{{name}}"

            [defaults]
            rule_timeout = "10s"  # python rules start slowly on a loaded runner; a test of a timeout sets its own (CI-FAST)
            retry_tick   = "1s"
            """) + extra)

    def write_machine(self, extra=""):
        (self.etc / "machine.toml").write_text(textwrap.dedent(f"""\
            machine = "{HOST}"

            [plugins.flag]
            kind      = "exec"
            path      = "{self.plugin}"
            sha256    = "{sha(self.plugin)}"
            contracts = [1]

            [plugins.hours]
            kind      = "exec"
            path      = "{HOURS_BIN}"
            sha256    = "{sha(HOURS_BIN)}"
            contracts = [1]

            [surfaces.reviewer]
            command = ["{self.surface}"]
            """) + extra)

    def write_office(self, name, kinds, tools, rules, default="open", extra=""):
        d = self.etc / name
        (d / "repos").write_text("org/app modes=read-only,job house=local\norg/acme modes=job scope=acme\n")
        (d / "signers").write_text(f"alice {_keys.pub('alice')} alice\nmanager {_keys.pub('manager')} manager\n")
        (d / "office.toml").write_text(textwrap.dedent(f"""\
            name     = "{name}"
            region   = "region-a"
            machine  = "{HOST}"
            user     = "{name}"
            owner    = "alice"
            tier     = "reader"
            acts_for = ""
            kinds    = {json.dumps(kinds)}
            profile  = "reviewer"
            surfaces = ["reviewer"]
            signers  = "signers"
            {extra}

            [holdings]
            tools = {json.dumps(tools)}
            repos = "repos"
            effects = ["push"]

            [rules]
            default = "{default}"
            """) + textwrap.dedent(rules))

    def flag_rule(self, name="flag", behave="file", binding=False, at=None, timeout=None, seen=None):
        s = f'\n[[rules.use]]\nname = "{name}"\nplugin = "flag"\n'
        if binding:
            s += "binding = true\n"
        if at:
            s += f"at = {json.dumps(at)}\n"
        if timeout:
            s += f'timeout = "{timeout}"\n'
        s += f'[rules.use.config]\nfile = "{self.flag}"\nbehave = "{behave}"\n'
        if seen:
            s += f'seen = "{seen}"\n'
        return s

    # ------------------------------------------------------------ running

    def run_bin(self, binary, *args, input=None, env=None):
        return subprocess.run([binary, *args], env={**self.env, **(env or {})}, input=input,
                              capture_output=True, text=True, timeout=60)

    def office(self, *args, **kw):
        return self.run_bin(OFFICE_BIN, *args, **kw)

    def bowery(self, *args, **kw):
        return self.run_bin(BIN, *args, **kw)

    def office_ok(self, *args, **kw):
        r = self.office(*args, **kw)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("dev mode:", r.stderr)  # said on every command
        return r.stdout

    def place(self, *args, code=None):
        r = self.bowery("place", *args)
        if code is not None:
            self.assertEqual(r.returncode, code, r.stdout + r.stderr)
        return json.loads(r.stdout), r

    def log(self, office):
        # The office's activity log and its decisions log, in time order.
        out = []
        for p in (self.var / office / "log", self.var / "decisions" / f"{office}.log"):
            if p.exists():
                out += [json.loads(l) for l in p.read_text().splitlines()]
        return sorted(out, key=lambda e: e["time"])

    def events(self, office, kind):
        return [e for e in self.log(office) if e["event"] == kind]

    def place_log(self):
        p = self.state / "place.log"
        return [json.loads(l) for l in p.read_text().splitlines()] if p.exists() else []

    def started_ids(self):
        return [l.split()[0] for l in self.started.read_text().splitlines()] if self.started.exists() else []

    def pull(self, office=None):
        return json.loads(self.office_ok("pull", office or self.REVIEWS, "--json"))

    def probe(self, office, job):
        return json.loads(self.office_ok("describe", office, "--probe", input=json.dumps(job)))

    def job(self, **kw):
        j = {"id": "j1", "kind": "review", "asked_by": "alice", "needs": {}, "effects": [], "tags": {},
             "expires": "2099-01-01T00:00:00Z"}
        j.update(kw)
        return j


class TheDoneWhen(Offices):
    """The plan's M1 story: a placement to a closed office waits and never
    runs; it's placed after office open or once its rule opens; a crashed or
    slow plugin closes the office; every verdict is logged."""

    def test_a_closed_office_keeps_the_job_waiting_and_office_open_places_it(self):
        self.office_ok("close", self.REVIEWS)
        out, r = self.place("--kind", "review", "--id", "rev-1", "--", "review PR 1", code=4)
        self.assertIn("closed by its owner", out["queued"]["waiting_for"])
        self.assertIn("alice", out["queued"]["told"])
        self.assertTrue((self.state / "queue" / "rev-1.json").exists())
        # Nothing reached the inbox, and a pull starts nothing.
        self.assertEqual(self.pull()["accepted"], [])
        self.assertEqual(self.started_ids(), [])
        # A retry while it's still closed tells nobody twice.
        again = json.loads(self.bowery("place", "--retry").stdout)
        self.assertEqual(again["waiting"][0]["told"], [])

        self.office_ok("open", self.REVIEWS)
        again = json.loads(self.bowery("place", "--retry").stdout)
        self.assertEqual(again["placed"][0]["office"], self.REVIEWS)
        self.assertFalse((self.state / "queue" / "rev-1.json").exists())
        rep = self.pull()
        self.assertEqual([a["id"] for a in rep["accepted"]], ["rev-1"])
        self.assertEqual(self.started_ids(), ["rev-1"])
        flips = [e["state"] for e in self.events(self.REVIEWS, "state")]
        self.assertEqual(flips, ["closed", "open"])
        self.assertEqual(self.place_log()[-1]["event"], "placed")

    def test_a_rule_that_opens_lets_the_waiting_job_through(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.set_flag("closed", reason="not now", retry_after="2099-01-01T00:00:00Z")
        out, _ = self.place("--kind", "review", "--id", "rev-2", code=4)
        self.assertEqual(out["queued"]["waiting_for"], f"{self.REVIEWS}: not now")
        self.assertEqual(out["queued"]["retry_after"], "2099-01-01T00:00:00Z")
        self.set_flag("open")
        out = json.loads(self.bowery("place", "--retry").stdout)
        self.assertEqual(out["placed"][0]["id"], "rev-2")
        self.pull()
        self.assertEqual(self.started_ids(), ["rev-2"])

    def test_a_rule_that_closes_after_placing_holds_it_in_the_inbox(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.place("--kind", "review", "--id", "rev-3", code=0)
        self.set_flag("closed", reason="lunch")
        rep = self.pull()
        self.assertEqual(rep["waiting"], [{"id": "rev-3", "reason": "lunch"}])
        self.assertEqual(self.started_ids(), [])
        self.assertEqual(len(self.events(self.REVIEWS, "place-waiting")), 1)
        self.pull()  # still closed: the wait isn't logged again
        self.assertEqual(len(self.events(self.REVIEWS, "place-waiting")), 1)
        self.set_flag("open")
        self.pull()
        self.assertEqual(self.started_ids(), ["rev-3"])

    def test_a_crashed_plugin_closes_the_office_and_is_logged(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule(behave="crash"))
        out, _ = self.place("--kind", "review", "--id", "rev-4", code=4)
        self.assertIn("rule flag failed: it exited 3", out["queued"]["waiting_for"])
        failed = self.events(self.REVIEWS, "rule-failed")
        self.assertEqual(failed[0]["instance"], "flag")
        self.assertEqual(failed[0]["alert"]["level"], "smoke")

    def test_a_slow_plugin_is_killed_at_its_timeout_and_closes(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule(behave="hang", timeout="500ms"))
        t0 = time.monotonic()
        res = self.probe(self.REVIEWS, self.job())
        self.assertLess(time.monotonic() - t0, 5)
        self.assertEqual(res["verdict"], "closed")
        # On a loaded machine python's start-up can pass 500ms in the plugin's
        # describe already: killed there or in the check, it's the same close.
        self.assertRegex(res["reason"], r"rule flag failed: (describe: )?no answer in time")

    def test_every_verdict_is_logged_one_line_per_instance(self):
        rules = self.flag_rule("one") + self.flag_rule("two") + textwrap.dedent("""
            [[rules.use]]
            name = "always"
            plugin = "hours"
            [rules.use.config]
            windows = ["Mon..Sun 00:00-24:00"]
            """)
        self.write_office(self.REVIEWS, ["review"], ["git"], rules)
        self.place("--kind", "review", "--id", "rev-5", code=0)
        self.pull()
        verdicts = self.events(self.REVIEWS, "verdict")
        by_phase = {}
        for v in verdicts:
            by_phase.setdefault(v["phase"], []).append(v["instance"])
            for k in ("time", "component", "job", "plugin", "plugin_version", "sha256", "contract", "verdict", "ms", "result"):
                self.assertIn(k, v)
        self.assertEqual(by_phase, {"place": ["one", "two", "always"], "accept": ["one", "two", "always"]})
        self.assertEqual(verdicts[0]["sha256"], sha(self.plugin))
        self.assertEqual({e["component"] for e in self.events(self.REVIEWS, "evaluated")}, {"resolver-probe", "pull"})
        placed = self.place_log()[-1]
        self.assertEqual(placed["office"], self.REVIEWS)
        self.assertTrue(any(c["office"] == self.BUILDER and "kind review" in c["reason"] for c in placed["candidates"]))


class Stages(Offices):
    def test_each_stage_is_timed_in_both_logs(self):
        out, _ = self.place("--kind", "review", "--id", "rev-st", code=0)
        self.assertEqual(set(out["placed"]["stages"]), {"resolve", "resolved", "sign", "send", "sent"})
        self.pull()
        launched = self.events(self.REVIEWS, "launched")[0]
        st = launched["stages"]
        stages = ("resolve", "sign", "send", "delivered", "verified", "accepted", "started")
        self.assertEqual(list(sorted(st)), sorted(stages))
        # "send" is the letter file's mtime, which the kernel stamps from a coarse clock that can read a few
        # milliseconds behind: a stage is never earlier than its predecessor by more than that.
        from datetime import datetime
        order = [datetime.fromisoformat(st[k].replace("Z", "+00:00")).timestamp() for k in stages]
        for (a, b), k in zip(zip(order, order[1:]), stages[1:]):
            self.assertGreaterEqual(b, a - 0.02, f"{k} is before the stage ahead of it: {st}")
        self.assertIn("stages", self.place_log()[-1])

    def test_the_pull_loop_takes_a_placement_within_its_interval(self):
        loop = subprocess.Popen([OFFICE_BIN, "pull", self.REVIEWS, "--loop", "--interval", "100ms", "--for", "20s"],
                                env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            time.sleep(0.5)
            self.place("--kind", "review", "--id", "rev-loop", code=0)
            deadline = time.monotonic() + 15  # under the loop's --for and the timer it rules out; generous for a loaded runner (CI-FAST)
            while time.monotonic() < deadline and "rev-loop" not in self.started_ids():
                time.sleep(0.1)
            self.assertEqual(self.started_ids(), ["rev-loop"])
        finally:
            loop.terminate()
            loop.wait(5)

    def test_the_pull_loop_notices_office_open_without_a_new_letter(self):
        # A placement in the box of an office closed since, as a resolver with
        # a stale view would leave it.
        self.place("--kind", "review", "--id", "rev-ev", code=0)
        self.office_ok("close", self.REVIEWS)
        loop = subprocess.Popen([OFFICE_BIN, "pull", self.REVIEWS, "--loop", "--interval", "100ms", "--backstop", "60s",
                                 "--for", "20s"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            time.sleep(1)
            self.assertEqual(self.started_ids(), [])
            self.office_ok("open", self.REVIEWS)
            deadline = time.monotonic() + 15  # under the loop's --for and the timer it rules out; generous for a loaded runner (CI-FAST)
            while time.monotonic() < deadline and not self.started_ids():
                time.sleep(0.1)
            self.assertEqual(self.started_ids(), ["rev-ev"])
        finally:
            loop.terminate()
            loop.wait(5)


class TheEvaluator(Offices):
    def test_no_rules_means_the_default(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], "", default="closed")
        res = self.probe(self.REVIEWS, self.job())
        self.assertEqual((res["verdict"], res["by"]), ("closed", "default"))
        self.write_office(self.REVIEWS, ["review"], ["git"], "", default="open")
        self.assertEqual(self.probe(self.REVIEWS, self.job())["verdict"], "open")

    def test_defer_drops_out_and_all_must_pass(self):
        defer_file = self.t / "defer.json"
        defer_file.write_text('{"verdict": "defer"}')
        rules = self.flag_rule("main") + f'\n[[rules.use]]\nname = "d"\nplugin = "flag"\n[rules.use.config]\nfile = "{defer_file}"\n'
        self.write_office(self.REVIEWS, ["review"], ["git"], rules, default="closed")
        self.set_flag("defer")
        self.assertEqual(self.probe(self.REVIEWS, self.job())["by"], "default")
        self.set_flag("open")
        self.assertEqual(self.probe(self.REVIEWS, self.job())["verdict"], "open")
        self.set_flag("closed", reason="no")
        self.assertEqual(self.probe(self.REVIEWS, self.job())["reason"], "no")

    def test_the_reason_is_the_first_closed_and_retry_is_the_latest(self):
        other = self.t / "other.json"
        rules = self.flag_rule("a") + f'\n[[rules.use]]\nname = "b"\nplugin = "flag"\n[rules.use.config]\nfile = "{other}"\n'
        self.write_office(self.REVIEWS, ["review"], ["git"], rules)
        self.set_flag("closed", reason="first", retry_after="2099-01-01T09:00:00Z")
        other.write_text('{"verdict": "closed", "reason": "second", "retry_after": "2099-01-02T09:00:00Z"}')
        res = self.probe(self.REVIEWS, self.job())
        self.assertEqual((res["reason"], res["retry_after"]), ("first", "2099-01-02T09:00:00Z"))
        other.write_text('{"verdict": "closed", "reason": "second"}')
        self.assertEqual(self.probe(self.REVIEWS, self.job())["retry_after"], "")

    def test_a_bad_answer_is_a_failed_rule(self):
        for behave, what in (("nonsense", "doesn't decode"), ("noreason", "closed without a reason"), ("extra", "unknown field")):
            self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule(behave=behave))
            res = self.probe(self.REVIEWS, self.job())
            self.assertEqual(res["verdict"], "closed", behave)
            self.assertIn(what, res["reason"], behave)
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.flag.write_text('{"verdict": "closed", "reason": "x", "retry_after": "tomorrow"}')
        self.assertIn("isn't RFC 3339", self.probe(self.REVIEWS, self.job())["reason"])

    def test_a_plugin_that_changed_since_it_was_pinned_fails(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.plugin.write_text(PLUGIN + "\n# tampered\n")
        res = self.probe(self.REVIEWS, self.job())
        self.assertIn("doesn't match its pinned sha256", res["reason"])

    def test_config_that_misses_the_schema_and_unknown_plugins_fail(self):
        self.write_office(self.REVIEWS, ["review"], ["git"],
                          '\n[[rules.use]]\nname = "x"\nplugin = "flag"\n[rules.use.config]\ncolour = "red"\n')
        self.assertIn("doesn't fit the plugin's schema", self.probe(self.REVIEWS, self.job())["reason"])
        self.write_office(self.REVIEWS, ["review"], ["git"], '\n[[rules.use]]\nname = "x"\nplugin = "nope"\n')
        self.assertIn("isn't installed", self.probe(self.REVIEWS, self.job())["reason"])

    def test_the_request_follows_the_contract(self):
        seen = self.t / "seen.jsonl"
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule(seen=str(seen)))
        self.place("--kind", "review", "--id", "rev-req", "--scope", "acme", "--tag", "x=y", code=0)
        self.pull()
        reqs = [json.loads(l) for l in seen.read_text().splitlines()]
        self.assertEqual([r["phase"] for r in reqs], ["place", "accept"])
        self.assertEqual([r["component"] for r in reqs], ["resolver-probe", "pull"])
        r = reqs[1]
        self.assertEqual(set(r), {"contract", "phase", "component", "rule", "job", "office", "machine", "clock"})
        self.assertEqual(r["job"]["id"], "rev-req")
        self.assertEqual(r["job"]["tags"], {"x": "y"})
        self.assertEqual(r["office"]["name"], self.REVIEWS)
        self.assertEqual(r["clock"]["timezone"], "UTC")
        self.assertEqual(r["rule"]["name"], "flag")
        self.assertEqual(r["machine"]["name"], HOST)
        # history 1h: the plugin sees what was accepted on this machine.
        self.place("--kind", "review", "--id", "rev-req2", code=0)
        self.pull()
        last = json.loads(seen.read_text().splitlines()[-1])
        self.assertEqual([a["id"] for a in last["machine"]["accepted"]], ["rev-req"])

    def test_an_instance_runs_only_in_its_phases(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule(at=["accept"]))
        self.set_flag("closed", reason="accept only")
        self.place("--kind", "review", "--id", "rev-ph", code=0)  # the place phase doesn't run it
        self.assertEqual(self.pull()["waiting"][0]["reason"], "accept only")

    def test_region_and_machine_instances_run_first(self):
        self.write_region('\n[[defaults.rules]]\nname = "regional"\nplugin = "flag"\n'
                          f'[defaults.rules.config]\nfile = "{self.flag}"\n')
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule("own"))
        res = self.probe(self.REVIEWS, self.job())
        self.assertEqual([i["instance"] for i in res["instances"]], ["regional", "own"])
        self.assertEqual([i["source"] for i in res["instances"]], ["region", "office"])


class Owner(Offices):
    def test_close_beats_every_rule_and_open_beats_all_but_binding(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.office_ok("close", self.REVIEWS)
        self.assertEqual(self.probe(self.REVIEWS, self.job())["by"], "owner")
        self.set_flag("closed", reason="rule says no")
        self.office_ok("open", self.REVIEWS)
        res = self.probe(self.REVIEWS, self.job())
        self.assertEqual((res["verdict"], res["by"]), ("open", "owner"))
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule(binding=True))
        res = self.probe(self.REVIEWS, self.job())
        self.assertEqual(res["verdict"], "closed")
        self.assertIn("binding", res["reason"])
        self.office_ok("clear", self.REVIEWS)
        self.assertEqual(self.probe(self.REVIEWS, self.job())["reason"], "rule says no")

    def test_until_kinds_and_scope_limit_the_override(self):
        self.office_ok("close", self.REVIEWS, "--kinds", "builder")
        self.assertEqual(self.probe(self.REVIEWS, self.job())["verdict"], "open")
        self.office_ok("close", self.REVIEWS, "--scope", "acme")
        self.assertEqual(self.probe(self.REVIEWS, self.job())["verdict"], "open")
        self.assertEqual(self.probe(self.REVIEWS, self.job(scope="acme"))["verdict"], "closed")
        # Wide enough that a loaded machine still probes inside it (1s
        # wasn't: the probe after the close ran past it at load 113).
        self.office_ok("close", self.REVIEWS, "--until", "5s")
        self.assertEqual(self.probe(self.REVIEWS, self.job())["verdict"], "closed")
        time.sleep(5.2)
        self.assertEqual(self.probe(self.REVIEWS, self.job())["verdict"], "open")

    def test_describe_and_ls_show_holdings_state_and_verdicts(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], textwrap.dedent("""
            [[rules.use]]
            name = "never"
            plugin = "hours"
            [rules.use.config]
            windows = ["Mon 00:00-00:01"]
            """))
        self.office_ok("describe", self.REVIEWS, "--probe", input=json.dumps(self.job()))
        d = json.loads(self.office_ok("describe", self.REVIEWS, "--json"))
        self.assertEqual(d["holdings"]["tools"], ["git"])
        self.assertEqual(d["repos"][0], {"repo": "org/app", "modes": ["read-only", "job"], "house": "local"})
        self.assertEqual(d["signers"], ["alice", "manager"])
        self.assertEqual(d["box"], "office-" + self.REVIEWS)
        self.assertEqual(d["mode"], "dev")
        if d["verdicts"]:  # unless the test runs in that one minute
            self.assertEqual(d["verdicts"][0]["instance"], "never")
        self.office_ok("close", self.REVIEWS)
        ls = json.loads(self.office_ok("ls", "--json"))
        self.assertEqual([o["office"] for o in ls], [self.BUILDER, self.REVIEWS])
        self.assertEqual(ls[1]["state"]["state"], "closed")
        text = self.bowery("office", "ls").stdout
        self.assertIn(self.REVIEWS, text)
        self.assertIn("closed", text)

    def test_office_log_since(self):
        self.office_ok("close", self.REVIEWS)
        out = self.office_ok("log", self.REVIEWS, "--since", "1h")
        self.assertIn("state", out)
        self.assertIn("by=" + pwd.getpwuid(os.getuid()).pw_name, out)
        self.assertEqual(self.office_ok("log", self.REVIEWS, "--since", "1h", env={"OFFICE_NOW": "2099-01-01T00:00:00Z"}).strip(), "")


class Resolver(Offices):
    def test_holdings_filter_and_the_reasons(self):
        out, _ = self.place("--kind", "builder", "--needs", "tools=go", "--id", "b1", code=0)
        self.assertEqual(out["placed"]["office"], self.BUILDER)
        skipped = {s["office"]: s["reason"] for s in out["placed"]["skipped"]}
        self.assertIn("doesn't take kind builder", skipped[self.REVIEWS])
        self.assertIn("own machine", skipped["region-a-faraway"])

    def test_no_office_holds_it_is_a_setup_gap_not_a_wait(self):
        out, r = self.place("--kind", "review", "--needs", "tools=chromium", code=1)
        self.assertIn("no office holds", out["unplaceable"]["reason"])
        self.assertFalse((self.state / "queue").exists() and any((self.state / "queue").iterdir()))

    def test_a_reserved_effect_is_refused_by_policy(self):
        out, _ = self.place("--kind", "review", "--effects", "spend", code=3)
        self.assertIn("reserved effect spend", out["refused"]["reason"])

    def test_scope_wall_repo_modes_and_signers(self):
        _, r = self.place("--kind", "review", "--needs", "repo=org/acme@job", code=1)
        self.assertIn("scope acme only", r.stdout)
        self.place("--kind", "review", "--needs", "repo=org/acme@job", "--scope", "acme", code=0)
        _, r = self.place("--kind", "review", "--needs", "repo=org/app@push", code=1)
        self.assertIn("isn't held in mode push", r.stdout)
        _, r = self.place("--kind", "review", "--asked-by", "mallory", code=1)
        self.assertIn("mallory isn't among its signers", r.stdout)

    def test_pin_prefer_and_the_tie_break(self):
        self.write_office(self.BUILDER, ["review", "builder"], ["git"], "")
        out, _ = self.place("--kind", "review", code=0)
        self.assertEqual(out["placed"]["office"], self.BUILDER)  # name order, both equal
        self.write_config(f'prefer = ["{self.REVIEWS}"]\n')
        out, _ = self.place("--kind", "review", code=0)
        self.assertEqual(out["placed"]["office"], self.REVIEWS)
        out, _ = self.place("--kind", "review", "--pin", self.BUILDER, code=0)
        self.assertEqual(out["placed"]["office"], self.BUILDER)
        self.office_ok("close", self.BUILDER)
        out, _ = self.place("--kind", "review", "--pin", self.BUILDER, code=4)  # a pinned job waits, never moves
        self.assertIn(self.BUILDER, out["queued"]["waiting_for"])

    def test_never_keeps_a_scope_in_its_regions(self):
        self.write_config('never = [{ scope = "acme", outside = ["region-b"] }]\n')
        _, r = self.place("--kind", "review", "--scope", "acme", code=1)
        self.assertIn("never: scope acme", r.stdout)

    def test_an_unknown_placement_key_is_refused(self):
        self.config.write_text('[placement]\nconsder = 1\n')
        r = self.bowery("place", "--kind", "review")
        self.assertEqual(r.returncode, 1)
        self.assertIn("unknown key", r.stderr)

    def test_expired_waiting_jobs_are_dropped_and_retry_orders_by_priority(self):
        self.office_ok("close", self.REVIEWS)
        self.place("--kind", "review", "--id", "low-1", "--priority", "low", code=4)
        self.place("--kind", "review", "--id", "high-1", "--priority", "high", code=4)
        self.place("--kind", "review", "--id", "soon", "--expires", "1s", code=4)
        time.sleep(1.2)
        self.office_ok("open", self.REVIEWS)
        out = json.loads(self.bowery("place", "--retry").stdout)
        self.assertEqual([p["id"] for p in out["placed"]], ["high-1", "low-1"])
        self.assertEqual(out["dropped"], [{"job": "soon", "reason": "expired while waiting"}])
        self.assertRaises(Exception, self.place, "--kind", "review", "--expires", "48h", code=0)

    def test_queue_lists_the_waiting(self):
        self.office_ok("close", self.REVIEWS)
        self.place("--kind", "review", "--id", "q-1", code=4)
        self.assertIn("q-1", self.bowery("queue").stdout)
        self.assertEqual(json.loads(self.bowery("queue", "--json").stdout)[0]["job"]["id"], "q-1")


class Dispatch(Offices):
    def test_job_flags_make_a_dispatch_a_placement(self):
        r = self.bowery("dispatch", "rev", "--job-kind", "review", "--office", self.REVIEWS, "--tag", "pr=12",
                        "--surface", "herdr:reviewer", "--", "review", "PR 12")
        self.assertEqual(r.returncode, 0, r.stderr)
        out = json.loads(r.stdout)
        self.assertEqual(out["placed"]["office"], self.REVIEWS)
        self.pull()
        self.assertEqual(len(self.started_ids()), 1)
        letter = next((self.var / self.REVIEWS / "accepted").iterdir()).read_text()
        self.assertIn('name = "rev"', letter)
        self.assertIn('prompt = "review PR 12"', letter)
        self.assertIn('tags.pr = "12"', letter)
        self.assertFalse(list(self.state.glob("rev.*.json")))  # no herdr record: the office starts the agent

    def test_a_closed_office_is_exit_4_and_herdr_flags_are_refused(self):
        self.office_ok("close", self.REVIEWS)
        r = self.bowery("dispatch", "rev", "--job-kind", "review", "--office", self.REVIEWS)
        self.assertEqual(r.returncode, 4, r.stderr)
        r = self.bowery("dispatch", "rev", "--job-kind", "review", "--split")
        self.assertEqual(r.returncode, 1)
        self.assertIn("places the job", r.stderr)
        r = self.bowery("dispatch", "rev", "--office", self.REVIEWS)
        self.assertIn("--job-kind", r.stderr)


class Pull(Offices):
    def drop(self, name, body):
        box = self.var / "boxes" / ("office-" + self.REVIEWS)
        box.mkdir(parents=True, exist_ok=True)
        (box / (name + ".placement")).write_text(body)

    def letter(self, signer="alice", **kw):
        """A placement letter, signed by signer's key (None: unsigned)."""
        fields = {"id": "x1", "kind": "review", "asked_by": "alice", "expires": "2099-01-01T00:00:00Z",
                  "signed_at": "2098-12-31T12:00:00Z",
                  "office": self.REVIEWS, "machine": HOST, "region": "region-a"}
        fields.update(kw)
        lines = "".join(f'{k} = {json.dumps(v)}\n' for k, v in sorted(fields.items()) if v is not None)
        return _keys.letter("```placement\n" + lines + "```\n", requester=signer)

    def test_the_office_checks_every_placement_again(self):
        self.drop("x1", self.letter(office=self.BUILDER))
        self.drop("x2", self.letter(id="x2", kind="builder"))
        self.drop("x3", self.letter(id="x3", **{"needs.tools": "chromium"}))
        self.drop("x4", "not a placement")
        self.drop("x5", self.letter(id="x5", expires="2001-01-01T00:00:00Z"))
        rep = self.pull()
        refused = {r["id"]: r["reason"] for r in rep["refused"]}
        self.assertIn("another office", refused["x1"])
        self.assertIn("doesn't take kind builder", refused["x2"])
        self.assertIn("lacks tool chromium", refused["x3"])
        self.assertEqual({d["id"] for d in rep["dropped"]}, {"x4", "x5"})
        self.assertEqual(self.started_ids(), [])
        reply = (self.var / "boxes" / "asker-alice" / "x2.reply").read_text()
        self.assertIn("refused x2", reply)

    def test_one_per_placement_id(self):
        self.drop("x1", self.letter())
        self.pull()
        self.drop("x1", self.letter())
        self.pull()
        self.assertEqual(self.started_ids(), ["x1"])
        self.assertEqual(len(self.events(self.REVIEWS, "place-duplicate")), 1)

    def test_check_finds_what_install_would_refuse(self):
        self.office_ok("check", self.REVIEWS)
        self.write_office(self.REVIEWS, ["review"], ["git"], '\n[[rules.use]]\nname = "x"\nplugin = "hours"\n'
                          '[rules.use.config]\nwindows = []\n', extra='')
        r = self.office("check", self.REVIEWS)
        self.assertEqual(r.returncode, 1)
        self.assertIn("at least 1 items", r.stderr)
        self.write_office(self.REVIEWS, ["review"], ["git"], "")
        text = (self.etc / self.REVIEWS / "office.toml").read_text().replace('effects = ["push"]', 'effects = ["push", "pr"]')
        (self.etc / self.REVIEWS / "office.toml").write_text(text)
        r = self.office("check", self.REVIEWS)
        self.assertIn("effect pr exceeds tier reader", r.stderr)

    BUS = textwrap.dedent("""
        [bus]
        retention = "240h"
        private   = ["private.*"]

        [bus.retain]
        "job.*" = "24h"

        [bus.seats.alice]
        publish = ["job.*", "private.*"]
        read    = ["*"]

        [peers.region-b]
        address     = "/srv/b"
        may_publish = ["job.*"]
        may_read    = ["card.answered"]
        """)

    def test_bus_topic_rules_are_checked_with_the_region(self):
        self.write_region(self.BUS)
        self.office_ok("check", self.REVIEWS)
        self.office_ok("describe", self.REVIEWS)
        for bad, why in [
            (self.BUS.replace('"job.*", "private.*"', '"job*"'), 'bus.seats.alice.publish: "job*"'),
            (self.BUS.replace('may_read    = ["card.answered"]', 'may_read = ["card"]'), 'peers.region-b.may_read: "card"'),
            (self.BUS.replace('"*"', '"*.done"'), '"*.done"'),
            (self.BUS.replace('retention = "240h"', 'retention = "a month"'), "bus.retention"),
            (self.BUS.replace('retention = "240h"', 'retention = "-1h"'), "bus.retention"),
            (self.BUS.replace('"job.*" = "24h"', '"job.*" = "soon"'), "bus.retain"),
            (self.BUS.replace('"job.*" = "24h"', '"job" = "24h"'), 'bus.retain: "job"'),
            (self.BUS.replace("[bus.seats.alice]", "[bus.seats.Alice]"), "bus.seats.Alice"),
            (self.BUS.replace('private   = ["private.*"]', 'private = ["*"]'), "bus.private"),
            (self.BUS.replace('retention = "240h"', 'retention = "240h"\nmay_read = ["x.y"]'), "may_read"),
        ]:
            with self.subTest(why=why):
                self.write_region(bad)
                r = self.office("check", self.REVIEWS)
                self.assertEqual(r.returncode, 1, r.stdout)
                self.assertIn(why, r.stderr)

    WEB = textwrap.dedent("""
        [bus.web]
        listen = "100.64.0.1:7450"
        host   = "node.example.ts.net"
        owner  = "alice@example.com"
        origin = "https://board.example.ts.net"
        seat   = "alice"
        """)

    def test_bus_web_is_checked_with_the_region(self):
        self.write_region(self.BUS + self.WEB)
        self.office_ok("check", self.REVIEWS)
        for bad, why in [
            (self.WEB.replace('listen = "100.64.0.1:7450"', 'listen = "100.64.0.1"'), "bus.web.listen"),
            (self.WEB.replace('listen = "100.64.0.1:7450"', 'listen = ":7450"'), "bus.web.listen"),
            (self.WEB.replace('listen = "100.64.0.1:7450"', 'listen = "0.0.0.0:7450"'), "bus.web.listen"),
            (self.WEB.replace('host   = "node.example.ts.net"\n', ''), "bus.web.host"),
            (self.WEB.replace('owner  = "alice@example.com"\n', ''), "bus.web.owner"),
            (self.WEB.replace('"https://board.example.ts.net"', '"*"'), "bus.web.origin"),
            (self.WEB.replace('"https://board.example.ts.net"', '"https://board.example.ts.net/app"'), "bus.web.origin"),
            (self.WEB.replace('seat   = "alice"', 'seat = "bob"'), "bus.web.seat: bob has no [bus.seats.bob]"),
            (self.WEB + 'cert = "/x.pem"\n', "unknown"),
        ]:
            with self.subTest(why=why):
                self.write_region(self.BUS + bad)
                r = self.office("check", self.REVIEWS)
                self.assertEqual(r.returncode, 1, r.stdout)
                self.assertIn(why, r.stderr)

    def test_the_machine_file_must_name_this_machine(self):
        self.write_machine()
        text = (self.etc / "machine.toml").read_text().replace(f'machine = "{HOST}"', 'machine = "box-1"', 1)
        (self.etc / "machine.toml").write_text(text)
        r = self.office("describe", self.REVIEWS)
        self.assertEqual(r.returncode, 1)
        self.assertIn("runs only on its own machine", r.stderr)

    def test_unknown_keys_in_a_record_are_refused(self):
        p = self.etc / self.REVIEWS / "office.toml"
        p.write_text("run_as = \"root\"\n" + p.read_text())
        r = self.office("describe", self.REVIEWS)
        self.assertIn("unknown key(s) run_as", r.stderr)

    def test_root_mode_without_its_files_fails_closed(self):
        env = dict(self.env)
        del env["OFFICE_ROOT"]
        r = subprocess.run([OFFICE_BIN, "describe", self.REVIEWS], env=env, capture_output=True, text=True)
        self.assertEqual(r.returncode, 1)
        self.assertNotIn("dev mode", r.stderr)
        r = subprocess.run([OFFICE_BIN, "ls"], env={**env, "OFFICE_NOW": "2099-01-01T00:00:00Z"}, capture_output=True, text=True)
        self.assertIn("dev mode only", r.stderr)


class Examples(Offices):
    def test_the_example_offices_are_valid(self):
        import shutil
        from _binary import ROOT
        shutil.rmtree(self.etc)
        shutil.copytree(ROOT / "examples" / "offices" / "etc", self.etc)
        for p in self.etc.rglob("*.toml"):
            p.write_text(p.read_text().replace('"box-1"', json.dumps(HOST)))
        d = json.loads(self.office_ok("describe", "region-a-reviews", "--json"))
        self.assertEqual(d["kinds"], ["review"])
        # The example's plugin isn't installed here, so its rule fails closed.
        res = self.probe("region-a-reviews", self.job())
        self.assertIn("rule work-hours failed", res["reason"])


class Hours(unittest.TestCase):
    def ask(self, now, windows, tz="UTC"):
        req = {"contract": 1, "phase": "accept", "rule": {"name": "h", "config": {"windows": windows}},
               "clock": {"now": now, "timezone": tz}}
        r = subprocess.run([HOURS_BIN], input=json.dumps(req), capture_output=True, text=True, env={})
        return r.returncode, (json.loads(r.stdout) if r.stdout else None)

    def test_describe(self):
        r = subprocess.run([HOURS_BIN], input='{"contract": 1, "describe": true}', capture_output=True, text=True, env={})
        d = json.loads(r.stdout)
        self.assertEqual((d["name"], d["contracts"], d["phases"]), ("hours", [1], ["place", "accept", "run"]))

    def test_inside_and_outside_a_weekday_window(self):
        # 2026-01-05 is a Monday.
        code, v = self.ask("2026-01-05T10:00:00Z", ["Mon..Fri 09:00-17:00"])
        self.assertEqual(v, {"verdict": "open", "holds": "office", "valid_until": "2026-01-05T17:00:00Z"})
        code, v = self.ask("2026-01-05T18:00:00Z", ["Mon..Fri 09:00-17:00"])
        self.assertEqual((v["verdict"], v["retry_after"]), ("closed", "2026-01-06T09:00:00Z"))
        code, v = self.ask("2026-01-09T18:00:00Z", ["Mon..Fri 09:00-17:00"])  # Friday evening
        self.assertEqual(v["retry_after"], "2026-01-12T09:00:00Z")

    def test_the_office_timezone_overnight_windows_and_lists(self):
        code, v = self.ask("2026-01-05T14:30:00Z", ["Mon 09:00-10:00"], tz="America/New_York")
        self.assertEqual(v["verdict"], "open")
        code, v = self.ask("2026-01-06T01:00:00Z", ["Mon 22:00-02:00"])
        self.assertEqual((v["verdict"], v["valid_until"]), ("open", "2026-01-06T02:00:00Z"))
        code, v = self.ask("2026-01-07T12:00:00Z", ["Mon,Wed 11:00-13:00"])
        self.assertEqual(v["verdict"], "open")
        code, v = self.ask("2026-01-05T23:00:00Z", ["Mon 20:00-24:00", "Tue 00:00-08:00"])
        self.assertEqual(v["valid_until"], "2026-01-06T08:00:00Z")  # back-to-back windows are one

    def test_a_bad_window_is_a_failure(self):
        code, v = self.ask("2026-01-05T10:00:00Z", ["Someday 09:00-17:00"])
        self.assertNotEqual(code, 0)


class Review(Pull):
    """Found in review of offices-m1: each of these failed before its fix."""

    def test_a_placement_with_no_signing_time_can_not_dodge_max_expires(self):
        body = self.letter(id="nosign", signed_at=None)  # signed as it is: no signing time
        self.assertNotIn("signed_at", body)
        self.drop("nosign", body)
        rep = self.pull()
        self.assertEqual(rep["accepted"], [])
        self.assertEqual([r["id"] for r in rep["refused"]], ["nosign"])
        self.assertEqual(self.started_ids(), [])

    def test_an_asker_name_can_not_steer_a_reply_out_of_the_boxes(self):
        self.drop("esc", self.letter(id="esc", asked_by="a/../../pwn", office=self.BUILDER))
        self.pull()
        self.assertFalse((self.var / "pwn").exists())
        self.assertFalse(list(self.var.rglob("esc.reply")))

    def test_an_owner_open_beats_a_published_closed_verdict(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.set_flag("closed", reason="rule says no", holds="office", valid_until="2099-01-01T00:00:00Z")
        self.place("--kind", "review", "--id", "pub-1", code=4)
        self.assertEqual(json.loads(self.office_ok("describe", self.REVIEWS, "--json"))["verdicts"][0]["verdict"], "closed")
        self.office_ok("open", self.REVIEWS)
        out, _ = self.place("--kind", "review", "--id", "pub-2", code=0)
        self.assertEqual(out["placed"]["office"], self.REVIEWS)

    def test_an_office_that_stops_answering_its_probe_makes_the_job_wait(self):
        wrap = self.lib / "office-wrap"
        wrap.write_text(f"#!/bin/sh\n[ \"$3\" = --probe ] && exit 1\nexec {OFFICE_BIN} \"$@\"\n")
        wrap.chmod(0o755)
        self.write_config(office_command=wrap)
        out, _ = self.place("--kind", "review", "--id", "mute-1", code=4)
        self.assertIn("queued", out)
        self.assertTrue((self.state / "queue" / "mute-1.json").exists())

    def test_a_state_file_with_a_bad_until_closes_rather_than_never_expiring(self):
        d = self.var / "owner" / self.REVIEWS  # the owner's state, out of the office's own directory
        d.mkdir(parents=True, exist_ok=True)
        (d / "state").write_text(json.dumps({"state": "open", "until": "tomorrow", "by": "alice", "at": "2026-01-01T00:00:00Z"}))
        res = self.probe(self.REVIEWS, self.job())
        self.assertEqual(res["verdict"], "closed")
        self.assertIn("state", res["reason"])

    def test_the_earliest_retry_is_the_earliest_in_time_not_in_text(self):
        for name, f, ra in ((self.REVIEWS, self.t / "f1.json", "2099-01-01T10:00:00+02:00"),
                            (self.BUILDER, self.t / "f2.json", "2099-01-01T09:00:00Z")):
            f.write_text(json.dumps({"verdict": "closed", "reason": "later", "retry_after": ra}))
            rule = f'\n[[rules.use]]\nname = "flag"\nplugin = "flag"\n[rules.use.config]\nfile = "{f}"\n'
            self.write_office(name, ["review"], ["git"], rule)
        out, _ = self.place("--kind", "review", "--id", "ra-1", code=4)
        self.assertEqual(out["queued"]["retry_after"], "2099-01-01T10:00:00+02:00")  # 08:00Z


class ADeferredProbe(Offices):
    """In root mode an office's probe judges only for its own user and its
    owners: anyone else placing into it (another office's user on the same
    machine) gets "deferred", since its verdicts aren't the office's and the
    recorder wouldn't take them. Dev mode runs as one user, so a wrapper
    answers the probe the way a root-mode office answers such a placer. The
    resolver plans over a deferred office from its catalog, and the office
    judges the signed placement when it arrives, as itself."""

    def defer_probes(self, *names):
        wrap = self.lib / "office-defer"
        cases = " ".join(names)
        wrap.write_text(textwrap.dedent(f"""\
            #!/bin/sh
            if [ "$1" = describe ] && [ "$3" = --probe ]; then
              for o in {cases}; do
                if [ "$2" = "$o" ]; then
                  cat >/dev/null
                  printf '{{"office":"%s","verdict":"deferred","reason":"placer is not its user","retry_after":"","by":"","instances":[]}}\\n' "$2"
                  exit 0
                fi
              done
            fi
            exec {OFFICE_BIN} "$@"
            """))
        wrap.chmod(0o755)
        self.write_config(office_command=wrap)

    def test_a_deferred_office_is_placed_into_and_judges_the_job_on_arrival(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.set_flag("closed", reason="not yet")
        self.defer_probes(self.REVIEWS)
        out, _ = self.place("--kind", "review", "--id", "def-1", code=0)
        self.assertEqual(out["placed"]["office"], self.REVIEWS)
        cand = [c for c in self.place_log()[-1]["candidates"] if c["office"] == self.REVIEWS][0]
        self.assertTrue(cand["deferred"])
        # The placer judged nothing for the office: no place-phase decision.
        self.assertEqual([e for e in self.events(self.REVIEWS, "evaluated") if e["phase"] == "place"], [])
        # The office judges it, as itself, and keeps it while its rule says no.
        rep = self.pull()
        self.assertEqual(rep["waiting"], [{"id": "def-1", "reason": "not yet"}])
        ev = self.events(self.REVIEWS, "evaluated")
        self.assertEqual([(e["phase"], e["component"], e["job"]) for e in ev], [("accept", "pull", "def-1")])
        self.set_flag("open")
        self.pull()
        self.assertEqual(self.started_ids(), ["def-1"])

    def test_a_deferred_office_its_owner_closed_still_makes_the_job_wait(self):
        self.office_ok("close", self.REVIEWS)
        self.defer_probes(self.REVIEWS)
        out, _ = self.place("--kind", "review", "--id", "def-2", code=4)
        self.assertIn("closed by its owner", out["queued"]["waiting_for"])
        self.assertEqual(self.pull()["accepted"], [])

    def test_an_office_that_judged_it_open_goes_before_a_deferred_one(self):
        # By name the builder would win the tie; it only defers, so the
        # office that said open takes the job.
        self.write_office(self.BUILDER, ["review"], ["git"], rules="")
        self.defer_probes(self.BUILDER)
        out, _ = self.place("--kind", "review", "--id", "def-4", code=0)
        self.assertEqual(out["placed"]["office"], self.REVIEWS)
        self.defer_probes(self.BUILDER, self.REVIEWS)
        out, _ = self.place("--kind", "review", "--id", "def-5", code=0)
        self.assertEqual(out["placed"]["office"], self.BUILDER)


if __name__ == "__main__":
    unittest.main()
