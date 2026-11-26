"""Offices: the decisions on M1's review leftovers, in dev mode.

1. An integrity failure (a sha mismatch, a missing pin) closes whatever the
   owner's override; a plain failure still yields to an owner's open.
2. The owner's state and the decisions log live where the office's user
   can't write them, apart from the office's activity log; the decisions
   go through `office record`, which takes decisions only.
3. Plugins run from the opened, hashed file, not re-opened by path.
4. A surface runs in its own process group; a timeout or the owner's stop
   kills the group.
5. A replay of an accepted id gets one reply, "already accepted".

(The root-mode halves of 2, 3 and 6 — ownership of every parent, the
office's uid refused, an environment built from nothing — are Go tests in
internal/offices, since root mode needs root-owned files.)

    python3 -m unittest discover tests
"""

import json
import os
import sys
import textwrap
import time
import unittest

import test_offices
from test_offices import Offices, sha

ARGV0 = f"""#!{sys.executable}
import json, sys
req = json.load(sys.stdin)
if req.get("describe"):
    print(json.dumps({{"name": "argv0", "version": "0.1.0", "contracts": [1], "phases": ["place", "accept", "run"],
        "facts": [], "history": "0s", "config_schema": {{}}}}))
    sys.exit(0)
print(json.dumps({{"verdict": "open", "note": sys.argv[0]}}))
"""

# A surface whose start leaves a child behind in its group and, with
# "hang", never returns.
GROUP_SURFACE = f"""#!{sys.executable}
import json, os, subprocess, sys, time
job = json.load(sys.stdin)
here = os.path.dirname(os.path.abspath(sys.argv[0]))
action = sys.argv[1]
if action in ("start", "hang"):
    child = subprocess.Popen(["sleep", "300"])
    with open(os.path.join(here, "child.pid"), "w") as f:
        f.write(str(child.pid))
    if action == "hang":
        time.sleep(300)
"""


def alive(pid):
    try:
        with open(f"/proc/{pid}/stat") as f:
            return f.read().split(")")[-1].split()[0] != "Z"
    except FileNotFoundError:
        return False


class Integrity(Offices):
    def test_a_tampered_plugin_closes_even_under_the_owners_open(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.plugin.write_text(self.plugin.read_text() + "\n# tampered\n")
        self.office_ok("open", self.REVIEWS)
        res = self.probe(self.REVIEWS, self.job())
        self.assertEqual(res["verdict"], "closed")
        self.assertIn("doesn't match its pinned sha256", res["reason"])
        self.assertIn("integrity", res["reason"])

    def test_a_missing_pin_closes_even_under_the_owners_open(self):
        self.write_machine()
        p = self.etc / "machine.toml"
        p.write_text(p.read_text().replace(f'sha256    = "{sha(self.plugin)}"', 'sha256    = ""', 1))
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.office_ok("open", self.REVIEWS)
        self.assertEqual(self.probe(self.REVIEWS, self.job())["verdict"], "closed")

    def test_a_plain_failure_still_yields_to_the_owners_open(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule(behave="crash"))
        self.office_ok("open", self.REVIEWS)
        self.assertEqual(self.probe(self.REVIEWS, self.job())["verdict"], "open")


class Decisions(Offices):
    def test_decisions_and_the_owners_state_live_apart_from_the_office(self):
        self.write_office(self.REVIEWS, ["review"], ["git"], self.flag_rule())
        self.office_ok("close", self.REVIEWS)
        self.office_ok("open", self.REVIEWS)
        self.place("--kind", "review", "--id", "d-1", code=0)
        self.pull()
        activity = [json.loads(l) for l in (self.var / self.REVIEWS / "log").read_text().splitlines()]
        decisions = [json.loads(l) for l in (self.var / "decisions" / f"{self.REVIEWS}.log").read_text().splitlines()]
        self.assertEqual({e["event"] for e in decisions}, {"state", "verdict", "evaluated"})
        self.assertFalse({"state", "verdict", "evaluated", "rule-failed"} & {e["event"] for e in activity})
        self.assertIn("place-accepted", {e["event"] for e in activity})
        # The owner's state is out of the office's own directory.
        self.assertTrue((self.var / "owner" / self.REVIEWS / "state").exists())
        self.assertFalse((self.var / self.REVIEWS / "state").exists())
        # office log reads both, in time order.
        lines = [json.loads(l) for l in self.office_ok("log", self.REVIEWS, "--json").splitlines()]
        self.assertEqual([e["time"] for e in lines], sorted(e["time"] for e in lines))
        self.assertEqual({e["event"] for e in lines}, {e["event"] for e in activity + decisions})

    def test_record_takes_decisions_only(self):
        r = self.office("record", self.REVIEWS, input=json.dumps({"event": "verdict", "instance": "x"}) + "\n")
        self.assertEqual(r.returncode, 0, r.stderr)
        r = self.office("record", self.REVIEWS, input=json.dumps({"event": "place-accepted", "job": "x"}) + "\n")
        self.assertEqual(r.returncode, 1)
        self.assertIn("isn't a decision", r.stderr)
        r = self.office("record", self.REVIEWS, input="not json\n")
        self.assertEqual(r.returncode, 1)


class FdExec(Offices):
    @unittest.skipUnless(sys.platform.startswith("linux"), "/proc/self/fd")
    def test_a_plugin_runs_from_the_hashed_file_not_its_path(self):
        p = self.lib / "argv0"
        p.write_text(ARGV0)
        p.chmod(0o755)
        self.write_machine(f'\n[plugins.argv0]\nkind = "exec"\npath = "{p}"\nsha256 = "{sha(p)}"\ncontracts = [1]\n')
        self.write_office(self.REVIEWS, ["review"], ["git"], '\n[[rules.use]]\nname = "a"\nplugin = "argv0"\n')
        res = self.probe(self.REVIEWS, self.job())
        self.assertEqual(res["verdict"], "open", res)
        note = res["instances"][0]["verdict"]["note"]
        self.assertTrue(note.startswith("/proc/self/fd/"), note)


@unittest.skipUnless(sys.platform.startswith("linux"), "reads /proc")
class ProcessGroups(Offices):
    def setUp(self):
        super().setUp()
        self.surface.write_text(GROUP_SURFACE)

    def surface_cfg(self, start, timeout=None):
        self.write_machine()
        p = self.etc / "machine.toml"
        cfg = f'command = ["{self.surface}", "{start}"]\nstop = ["{self.surface}", "stop"]'
        if timeout:
            cfg += f'\ntimeout = "{timeout}"'
        p.write_text(p.read_text().replace(f'command = ["{self.surface}"]', cfg))

    def child(self):
        return int((self.lib / "child.pid").read_text())

    def wait_dead(self, pid):
        deadline = time.monotonic() + 20  # under the loop's --for and the timer it rules out; generous for a loaded runner (CI-FAST)
        while time.monotonic() < deadline and alive(pid):
            time.sleep(0.05)
        return not alive(pid)

    def test_a_launch_that_times_out_is_killed_with_its_group(self):
        self.surface_cfg("hang", timeout="1s")
        self.place("--kind", "review", "--id", "pg-1", code=0)
        t0 = time.monotonic()
        rep = self.pull()
        self.assertLess(time.monotonic() - t0, 10)
        self.assertIn("timed out", rep["accepted"][0]["reason"])
        self.assertTrue(self.wait_dead(self.child()), "the surface's child outlived its timeout")

    def test_the_owners_stop_kills_the_group(self):
        self.surface_cfg("start")
        self.place("--kind", "review", "--id", "pg-2", code=0)
        self.pull()
        pid = self.child()
        self.assertTrue(alive(pid))
        self.office_ok("stop", self.REVIEWS, "pg-2")
        self.pull()
        self.assertTrue(self.wait_dead(pid), "the job's group outlived the owner's stop")


class Replay(Offices):
    drop = test_offices.Pull.drop
    letter = test_offices.Pull.letter

    def test_a_replay_of_an_accepted_id_gets_one_reply(self):
        self.drop("x1", self.letter())
        self.pull()
        self.drop("x1", self.letter())
        self.pull()
        reply = (self.var / "boxes" / "asker-alice" / "x1.reply").read_text()
        self.assertIn("already accepted", reply)
        self.drop("x1", self.letter())
        self.pull()
        self.assertEqual(len(self.events(self.REVIEWS, "place-duplicate")), 2)
        self.assertEqual(self.events(self.REVIEWS, "reply-failed"), [])
        self.assertEqual(self.started_ids(), ["x1"])


if __name__ == "__main__":
    unittest.main()
