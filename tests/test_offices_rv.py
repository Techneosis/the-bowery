"""Offices M2: the review's findings, in dev mode. Each test was seen failing
before its fix.

1. A gate dated in the future (clock skew, or a touched file) is never
   stale, so it must be closed, not open for ever.
2. A waiting placement whose retry_after is already past (a gate whose
   next_slot lags) must not make the pull loop spin.
3. An event that lands while a pass is judging (a gate that opens mid-pass)
   must be seen by the long-poll, not slept through to the backstop.
4. `office done` while the run phase is judging that job must not be undone
   by the pass writing its stale copy back.

    python3 -m unittest discover tests
"""

import json
import os
import subprocess
import sys
import time
import unittest
from datetime import datetime, timedelta, timezone

from _binary import OFFICE_BIN
from test_offices import HOST, sha
from test_offices_run import Run, iso


class Drops:
    def drop(self, id):
        fields = {"id": id, "kind": "review", "asked_by": "alice", "expires": "2099-01-01T00:00:00Z",
                  "signed_at": "2098-12-31T12:00:00Z", "office": self.REVIEWS, "machine": HOST, "region": "region-a"}
        body = "```placement\n" + "".join(f"{k} = {json.dumps(v)}\n" for k, v in sorted(fields.items())) + "```\n"
        box = self.var / "boxes" / ("office-" + self.REVIEWS)
        box.mkdir(parents=True, exist_ok=True)
        tmp = box / f".{id}.tmp"
        tmp.write_text(body)
        tmp.rename(box / f"{id}.placement")


def future_gate(t, secs):
    os.utime(t, (time.time() + secs, time.time() + secs))


class FutureGate(Run):
    def probe_reason(self):
        return self.probe(self.REVIEWS, self.job())

    def test_a_gate_dated_in_the_future_is_closed_not_fresh_for_ever(self):
        self.office_with(self.gate_rule())
        # Its writer's clock, and its mtime, are an hour ahead: age is
        # negative, which is never over max_age.
        self.set_gate(True, age=-3600)
        res = self.probe_reason()
        self.assertEqual(res["verdict"], "closed")
        self.assertIn("future", res["reason"])

    def test_a_gate_whose_mtime_alone_is_in_the_future_is_closed(self):
        self.gate_only_mtime()
        self.office_with(self.gate_rule())
        self.set_gate(True)
        future_gate(self.gate, 3600)
        res = self.probe_reason()
        self.assertEqual(res["verdict"], "closed")
        self.assertIn("future", res["reason"])

    def gate_only_mtime(self):
        self.write_machine(self.gate_plugins(at_field=False))

    def test_a_little_skew_is_tolerated(self):
        self.office_with(self.gate_rule())
        self.set_gate(True, age=-1)
        self.assertEqual(self.probe_reason()["verdict"], "open")


class PastRetry(Drops, Run):
    def test_a_retry_that_already_passed_does_not_spin_the_loop(self):
        self.office_with(self.gate_rule())
        past = iso(datetime.now(timezone.utc) - timedelta(hours=1))
        self.set_gate(False, reason="the slot lags", next_slot=past)
        self.drop("pr-1")
        loop = subprocess.Popen([OFFICE_BIN, "pull", self.REVIEWS, "--loop", "--interval", "100ms", "--backstop", "60s",
                                 "--for", "3s"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            loop.wait(10)
        finally:
            if loop.poll() is None:
                loop.terminate()
                loop.wait(5)
            loop.stdout.close()
            loop.stderr.close()
        passes = [e for e in self.log(self.REVIEWS) if e["event"] in ("evaluated", "verdict") and e.get("phase") == "accept"]
        # One pass for the letter, and at most a few more for the events:
        # never one every few milliseconds.
        self.assertLess(len(passes), 12, f"{len(passes)} accept-phase lines in 3s")


# A rule that defers, and on its first run (when armed) opens the gate a
# moment later: an event that lands while the pass is still judging.
FLIPPER = f"""#!{sys.executable}
import json, os, sys, time
req = json.load(sys.stdin)
if req.get("describe"):
    print(json.dumps({{"name": "flipper", "version": "0.1.0", "contracts": [1], "phases": ["place", "accept", "run"],
        "facts": [], "history": "0s",
        "config_schema": {{"type": "object", "properties": {{"arm": {{"type": "string"}}, "gate": {{"type": "string"}}}}}}}}))
    sys.exit(0)
cfg = req["rule"]["config"]
if os.path.exists(cfg["arm"]):
    os.remove(cfg["arm"])
    time.sleep(0.6)
    with open(cfg["gate"], "w") as f:
        json.dump({{"run": True, "reason": "", "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}}, f)
print(json.dumps({{"verdict": "defer"}}))
"""


class MidPassEvent(Drops, Run):
    def test_a_gate_that_opens_while_a_pass_is_judging_is_not_slept_through(self):
        flipper = self.lib / "flipper"
        flipper.write_text(FLIPPER)
        flipper.chmod(0o755)
        arm = self.t / "arm"
        p = self.etc / "machine.toml"
        p.write_text(p.read_text() + f'\n[plugins.flipper]\nkind = "exec"\npath = "{flipper}"\n'
                     f'sha256 = "{sha(flipper)}"\ncontracts = [1]\n')
        self.office_with(self.gate_rule() + '\n[[rules.use]]\nname = "flip"\nplugin = "flipper"\n'
                         f'config = {{ arm = "{arm}", gate = "{self.gate}" }}\n')
        self.set_gate(False, reason="not yet")
        loop = subprocess.Popen([OFFICE_BIN, "pull", self.REVIEWS, "--loop", "--interval", "100ms", "--backstop", "60s",
                                 "--for", "30s"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            time.sleep(1)  # the loop is blocked on an empty inbox
            arm.write_text("x")
            self.drop("mp-1")
            deadline = time.monotonic() + 8
            while time.monotonic() < deadline and not self.started_ids():
                time.sleep(0.1)
            self.assertEqual(self.started_ids(), ["mp-1"])
        finally:
            loop.terminate()
            loop.wait(5)
            loop.stdout.close()
            loop.stderr.close()


# A run-phase rule that closes, and when armed first lets the job end: the
# agent's `office done` landing while the pass is still judging.
ENDER = f"""#!{sys.executable}
import json, os, sys, time
req = json.load(sys.stdin)
if req.get("describe"):
    print(json.dumps({{"name": "ender", "version": "0.1.0", "contracts": [1], "phases": ["run"],
        "facts": [], "history": "0s",
        "config_schema": {{"type": "object", "properties": {{"arm": {{"type": "string"}}, "record": {{"type": "string"}}}}}}}}))
    sys.exit(0)
cfg = req["rule"]["config"]
if os.path.exists(cfg["arm"]):
    time.sleep(0.5)
    os.remove(cfg["record"])
    os.remove(cfg["arm"])
print(json.dumps({{"verdict": "closed", "reason": "closing time"}}))
"""


class DoneDuringPass(Run):
    def test_a_job_that_ends_while_the_run_phase_judges_it_is_not_held_again(self):
        ender = self.lib / "ender"
        ender.write_text(ENDER)
        ender.chmod(0o755)
        arm = self.t / "arm"
        record = self.var / self.REVIEWS / "running" / "dd-1.json"
        p = self.etc / "machine.toml"
        p.write_text(p.read_text() + f'\n[plugins.ender]\nkind = "exec"\npath = "{ender}"\n'
                     f'sha256 = "{sha(ender)}"\ncontracts = [1]\n')
        self.office_with('\n[[rules.use]]\nname = "end"\nplugin = "ender"\nat = ["run"]\n'
                         f'config = {{ arm = "{arm}", record = "{record}" }}\n')
        self.start("dd-1")
        arm.write_text("x")
        rep = self.pull()
        self.assertEqual(rep["run"], [], rep)
        self.assertNotIn("dd-1", self.running())
        self.assertEqual(self.surface_calls("hold"), [])


if __name__ == "__main__":
    unittest.main()
