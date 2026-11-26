"""Offices, milestone 2: rules on the machine, in dev mode.

The state-file kind (a gate a service outside the office host writes, read
fail-closed: missing, malformed or stale is closed), facts, the run phase
and on_closed (yield, finish-turn, stop-next-turn, and the owner's
close --now) holding running work in place and resuming it, logged owner
overrides, and the pull loop as a long-poll with a backstop.

The gate here has the shape of a real one: `run`, `reason`, `idle_s`,
`in_slot`, `next_slot`, `mem_available_gb`, rewritten every few seconds by
its writer.

    python3 -m unittest discover tests
"""

import json
import os
import subprocess
import sys
import textwrap
import time
import unittest
from datetime import datetime, timedelta, timezone

from _binary import OFFICE_BIN
from test_offices import HOST, Offices, sha

# A surface that starts, holds, resumes and stops: each action appends
# "<id> <action> <on_closed> <reason>" to lib/surface.log.
SURFACE = f"""#!{sys.executable}
import json, os, sys
job = json.load(sys.stdin)
action = sys.argv[1] if len(sys.argv) > 1 else "start"
here = os.path.dirname(os.path.abspath(sys.argv[0]))
with open(os.path.join(here, "surface.log"), "a") as f:
    f.write(" ".join([job["id"], action, os.environ.get("OFFICE_ON_CLOSED", "-"),
                      os.environ.get("OFFICE_REASON", "-")]) + "\\n")
if action == "start":
    with open(os.path.join(here, "started"), "a") as f:
        f.write(job["id"] + " " + os.environ["OFFICE_NAME"] + "\\n")
"""

# A rule that reads facts: it declares three, records the request it got,
# and answers from its flag file.
FACTFUL = f"""#!{sys.executable}
import json, sys
req = json.load(sys.stdin)
if req.get("describe"):
    print(json.dumps({{"name": "factful", "version": "0.1.0", "contracts": [1], "phases": ["place", "accept", "run"],
        "facts": ["load", "idle_s", "absent"], "history": "0s",
        "config_schema": {{"type": "object", "properties": {{"seen": {{"type": "string"}}, "file": {{"type": "string"}}}}}}}}))
    sys.exit(0)
cfg = req["rule"]["config"]
with open(cfg["seen"], "a") as f:
    f.write(json.dumps(req) + "\\n")
print(open(cfg["file"]).read())
"""

PROVIDER = f"""#!{sys.executable}
import json, os, sys
mode = open("@MODE@").read().strip()  # plugins run as /proc/self/fd/3: no path of their own
if mode == "crash":
    sys.exit(2)
if mode == "extra":
    print(json.dumps({{"load": 0.5, "uptime": 9}}))
else:
    print(json.dumps({{"load": 0.5}}))
"""


def iso(dt):
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


class Run(Offices):
    def setUp(self):
        super().setUp()
        self.surface.write_text(SURFACE)
        self.gate = self.t / "gate.json"
        self.surface_log = self.lib / "surface.log"
        self.factful = self.lib / "factful"
        self.factful.write_text(FACTFUL)
        self.factful.chmod(0o755)
        self.provider = self.lib / "provider"
        self.provider.write_text(PROVIDER.replace("@MODE@", str(self.lib / "provider.mode")))
        self.provider.chmod(0o755)
        (self.lib / "provider.mode").write_text("ok")
        self.set_gate(True)
        self.write_machine(self.gate_plugins())

    def gate_plugins(self, at_field=True, max_age="60s"):
        s = textwrap.dedent(f"""
            [plugins.desk-gate]
            kind              = "state-file"
            path              = "{self.gate}"
            max_age           = "{max_age}"
            open_field        = "run"
            reason_field      = "reason"
            retry_after_field = "next_slot"
            """)
        if at_field:
            s += 'at_field          = "at"\n'
        s += textwrap.dedent(f"""\
            [plugins.desk-gate.facts]
            idle_s           = "idle_s"
            mem_available_gb = "mem_available_gb"

            [plugins.factful]
            kind      = "exec"
            path      = "{self.factful}"
            sha256    = "{sha(self.factful)}"
            contracts = [1]

            [facts.probe]
            path     = "{self.provider}"
            sha256   = "{sha(self.provider)}"
            interval = "30s"
            facts    = ["load"]

            [surfaces.reviewer.more]
            """).replace("[surfaces.reviewer.more]\n", "")
        return s

    def write_machine(self, extra=""):
        # The base machine file, with the surface's hold, resume and stop.
        super().write_machine(extra)
        p = self.etc / "machine.toml"
        p.write_text(p.read_text().replace(
            f'command = ["{self.surface}"]',
            f'command = ["{self.surface}", "start"]\nhold = ["{self.surface}", "hold"]\n'
            f'resume = ["{self.surface}", "resume"]\nstop = ["{self.surface}", "stop"]'))

    def set_gate(self, run, reason="", age=0, at_age=None, next_slot=None, **extra):
        now = datetime.now(timezone.utc)
        g = {"run": run, "reason": reason, "idle_s": 900, "in_slot": run, "mem_available_gb": 22.5,
             "next_slot": next_slot, "at": iso(now - timedelta(seconds=age if at_age is None else at_age))}
        g.update(extra)
        self.gate.write_text(json.dumps(g))
        t = time.time() - age
        os.utime(self.gate, (t, t))

    def gate_rule(self, name="gate", on_closed=None, at=None):
        s = f'\n[[rules.use]]\nname = "{name}"\nplugin = "desk-gate"\n'
        if on_closed:
            s += f'on_closed = "{on_closed}"\n'
        if at:
            s += f"at = {json.dumps(at)}\n"
        return s

    def office_with(self, rules, on_closed=None, interval=None):
        extra = ""
        self.write_office(self.REVIEWS, ["review"], ["git"], rules)
        p = self.etc / self.REVIEWS / "office.toml"
        text = p.read_text()
        if on_closed:
            text = text.replace('[rules]\n', f'[rules]\non_closed = "{on_closed}"\n')
        if interval:
            text = text.replace('[rules]\n', f'[rules]\ninterval = "{interval}"\n')
        p.write_text(text + extra)

    def surface_calls(self, action=None):
        if not self.surface_log.exists():
            return []
        rows = [l.split(" ", 3) for l in self.surface_log.read_text().splitlines()]
        return [r for r in rows if action is None or r[1] == action]

    def running(self):
        d = json.loads(self.office_ok("describe", self.REVIEWS, "--json"))
        return {j["id"]: j for j in d["jobs"]}

    def start(self, id):
        self.place("--kind", "review", "--id", id, code=0)
        rep = self.pull()
        self.assertIn(id, [a["id"] for a in rep["accepted"]])
        self.assertEqual(self.running()[id]["state"], "running")


class TheDoneWhen(Run):
    """The plan's M2 story: a job outside the gate's window waits and starts
    when it opens; a stale gate file closes the office and holds running
    work, which resumes."""

    def test_a_job_outside_the_gates_window_waits_and_starts_when_it_opens(self):
        self.office_with(self.gate_rule())
        slot = iso(datetime.now(timezone.utc) + timedelta(hours=3))
        self.set_gate(False, reason="outside the slot", next_slot=slot)
        out, _ = self.place("--kind", "review", "--id", "g-1", code=4)
        self.assertEqual(out["queued"]["waiting_for"], f"{self.REVIEWS}: outside the slot")
        self.assertEqual(out["queued"]["retry_after"], slot)
        self.set_gate(True)
        out = json.loads(self.bowery("place", "--retry").stdout)
        self.assertEqual(out["placed"][0]["id"], "g-1")
        self.pull()
        self.assertEqual(self.started_ids(), ["g-1"])

    def test_a_placement_in_the_box_starts_when_the_gate_opens_under_the_pull_loop(self):
        self.office_with(self.gate_rule())
        self.place("--kind", "review", "--id", "g-2", code=0)
        self.set_gate(False, reason="the owner is at the desk")
        loop = subprocess.Popen([OFFICE_BIN, "pull", self.REVIEWS, "--loop", "--interval", "100ms", "--backstop", "60s",
                                 "--for", "30s"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            time.sleep(1)
            self.assertEqual(self.started_ids(), [])
            self.assertEqual(self.events(self.REVIEWS, "place-waiting")[0]["reason"], "the owner is at the desk")
            self.set_gate(True)  # the writer's next write
            deadline = time.monotonic() + 20  # under the loop's --for and the timer it rules out; generous for a loaded runner (CI-FAST)
            while time.monotonic() < deadline and not self.started_ids():
                time.sleep(0.1)
            self.assertEqual(self.started_ids(), ["g-2"])
        finally:
            loop.terminate()
            loop.wait(5)
            loop.stdout.close()
            loop.stderr.close()

    def test_a_stale_gate_holds_running_work_which_resumes_with_its_claimed_work(self):
        self.office_with(self.gate_rule())
        self.start("g-3")
        # The writer stops: the file goes stale, and the office closes.
        self.set_gate(True, age=120)
        rep = self.pull()
        self.assertEqual(rep["run"][0]["id"], "g-3")
        self.assertEqual(rep["run"][0]["action"], "held")
        self.assertEqual(rep["run"][0]["on_closed"], "yield")
        self.assertIn("rule gate failed: its state file is stale", rep["run"][0]["reason"])
        held = self.events(self.REVIEWS, "held")[0]
        self.assertEqual((held["job"], held["on_closed"], held["by"], held["told"]), ("g-3", "yield", "gate", True))
        self.assertEqual(self.surface_calls("hold")[0][:3], ["g-3", "hold", "yield"])
        failed = [e for e in self.events(self.REVIEWS, "rule-failed") if e["phase"] == "run"]
        self.assertEqual(failed[0]["alert"]["level"], "smoke")
        # Held work stays put: its record, its placement; nothing re-placed.
        self.assertEqual(self.running()["g-3"]["state"], "held")
        self.assertTrue((self.var / self.REVIEWS / "accepted" / "g-3.placement").exists())
        self.office_ok("yielded", self.REVIEWS, "g-3")
        self.assertEqual(self.events(self.REVIEWS, "yielded")[0]["job"], "g-3")
        # New work pauses while it is held, even with the gate fresh again
        # before the pass: the resume comes first, then the new job.
        self.place("--kind", "review", "--id", "g-4", code=4)  # the resolver sees the stale gate too
        self.set_gate(True)
        out = json.loads(self.bowery("place", "--retry").stdout)
        self.assertEqual(out["placed"][0]["id"], "g-4")
        self.set_gate(True, age=120)
        rep = self.pull()
        self.assertEqual(rep["waiting"][0]["id"], "g-4")
        self.assertIn("holding g-3", rep["waiting"][0]["reason"])
        self.set_gate(True)
        rep = self.pull()
        self.assertEqual(rep["run"], [{"id": "g-3", "action": "resumed"}])
        self.assertEqual([a["id"] for a in rep["accepted"]], ["g-4"])
        self.assertEqual(self.surface_calls("resume")[0][0], "g-3")
        resumed = self.events(self.REVIEWS, "resumed")[0]
        self.assertEqual(resumed["was"], "yield")
        self.assertEqual(self.running()["g-3"]["state"], "running")
        # Started once: the resume is the same job, not a new start.
        self.assertEqual(self.started_ids(), ["g-3", "g-4"])
        self.office_ok("done", self.REVIEWS, "g-3")
        self.assertNotIn("g-3", self.running())
        self.assertEqual(self.events(self.REVIEWS, "done")[0]["job"], "g-3")


class StateFile(Run):
    def probe_reason(self):
        return self.probe(self.REVIEWS, self.job())

    def test_missing_unreadable_and_malformed_gates_are_closed(self):
        self.office_with(self.gate_rule())
        self.assertEqual(self.probe_reason()["verdict"], "open")
        self.gate.unlink()
        res = self.probe_reason()
        self.assertEqual(res["verdict"], "closed")
        self.assertIn("rule gate failed: its state file", res["reason"])
        self.assertIn("is missing", res["reason"])
        for body, what in (("not json", "isn't a JSON object"), ('{"run": "yes", "at": "%s"}', "run isn't true or false"),
                           ('{"reason": "x", "at": "%s"}', "has no run"),
                           ('{"run": false, "reason": 3, "at": "%s"}', "reason isn't text"),
                           ('{"run": false, "next_slot": "soon", "at": "%s"}', 'next_slot "soon" isn\'t RFC 3339'),
                           ('{"run": true, "at": "%s"} {}', "trailing data"),
                           ('{"run": true}', "has no at")):
            self.gate.write_text(body.replace("%s", iso(datetime.now(timezone.utc))))
            res = self.probe_reason()
            self.assertEqual(res["verdict"], "closed", body)
            self.assertIn(what, res["reason"], body)
        self.gate.unlink()
        self.gate.mkdir()
        self.assertIn("isn't a regular file", self.probe_reason()["reason"])

    def test_stale_by_mtime_or_by_its_own_timestamp_whichever_is_older(self):
        self.office_with(self.gate_rule())
        # The bound is 60s. Ages well either side of it (not 59 and 61): the
        # probe itself takes time, over a second on a loaded CI runner.
        self.set_gate(True, age=30)
        self.assertEqual(self.probe_reason()["verdict"], "open")
        self.set_gate(True, age=90)
        self.assertIn("stale", self.probe_reason()["reason"])
        # Touched by something, but its writer's own clock stopped.
        self.set_gate(True, age=0, at_age=300)
        self.assertIn("stale", self.probe_reason()["reason"])
        # Unix seconds work too.
        self.set_gate(True, at=time.time())
        self.assertEqual(self.probe_reason()["verdict"], "open")

    def test_closed_carries_the_reason_and_retry_and_is_published_office_wide(self):
        self.office_with(self.gate_rule())
        slot = iso(datetime.now(timezone.utc) + timedelta(hours=1))
        self.set_gate(False, reason="typing at the desk", next_slot=slot)
        res = self.probe_reason()
        self.assertEqual((res["verdict"], res["reason"], res["retry_after"]), ("closed", "typing at the desk", slot))
        d = json.loads(self.office_ok("describe", self.REVIEWS, "--json"))
        self.assertEqual(d["verdicts"][0]["instance"], "gate")
        self.assertEqual(d["verdicts"][0]["verdict"], "closed")
        # Valid until the file would go stale, no longer.
        vu = datetime.fromisoformat(d["verdicts"][0]["valid_until"].replace("Z", "+00:00"))
        self.assertLess(vu, datetime.now(timezone.utc) + timedelta(seconds=61))
        # No reason in the file still closes, with one of its own.
        self.set_gate(False)
        self.assertEqual(self.probe_reason()["reason"], "desk-gate is closed")
        # The log line carries the file's mtime where an exec rule's sha256 goes.
        v = self.events(self.REVIEWS, "verdict")[-1]
        self.assertEqual(v["plugin"], "desk-gate")
        self.assertIn("state_mtime", v)

    def test_a_state_file_rule_takes_no_config_and_needs_max_age(self):
        self.office_with(self.gate_rule() + "[rules.use.config]\nx = 1\n")
        self.assertIn("takes no config", self.probe_reason()["reason"])
        r = self.office("check", self.REVIEWS)
        self.assertIn("takes no config", r.stderr)
        self.write_machine(self.gate_plugins().replace('max_age           = "60s"\n', ""))
        r = self.office("describe", self.REVIEWS)
        self.assertEqual(r.returncode, 1)
        self.assertIn("max_age is required", r.stderr)


class OnClosed(Run):
    def test_the_instance_then_office_then_region_decides(self):
        self.office_with(self.gate_rule(on_closed="finish-turn"), on_closed="stop-next-turn")
        self.start("o-1")
        self.set_gate(False, reason="slot over")
        rep = self.pull()
        self.assertEqual(rep["run"][0]["on_closed"], "finish-turn")
        self.assertEqual(self.surface_calls("hold")[0][2:], ["finish-turn", "slot over"])

    def test_the_owners_close_asks_the_offices_on_closed(self):
        self.office_with("", on_closed="finish-turn")
        self.start("o-2")
        self.office_ok("close", self.REVIEWS)
        rep = self.pull()
        self.assertEqual(rep["run"][0]["on_closed"], "finish-turn")
        self.assertEqual(self.events(self.REVIEWS, "held")[0]["by"], "owner")
        self.office_ok("open", self.REVIEWS)
        self.assertEqual(self.pull()["run"], [{"id": "o-2", "action": "resumed"}])

    def test_the_regions_default_and_then_yield(self):
        self.write_region('on_closed = "finish-turn"\n')
        self.office_with("")
        self.start("o-3")
        self.office_ok("close", self.REVIEWS)
        self.assertEqual(self.pull()["run"][0]["on_closed"], "finish-turn")

    def test_stop_next_turn_ends_the_job(self):
        self.office_with(self.gate_rule(on_closed="stop-next-turn"))
        self.start("o-4")
        self.set_gate(False, reason="the slot ended")
        rep = self.pull()
        self.assertEqual(rep["run"][0]["action"], "stopping")
        self.assertEqual(self.events(self.REVIEWS, "stopped")[0]["on_closed"], "stop-next-turn")
        self.assertEqual(self.running()["o-4"]["state"], "stopping")
        # Stopping isn't held: new work isn't paused behind it.
        self.set_gate(True)
        self.assertEqual(self.pull()["run"], [])
        self.office_ok("done", self.REVIEWS, "o-4")
        self.assertEqual(self.running(), {})

    def test_a_rule_that_leaves_out_run_never_stops_running_work(self):
        self.office_with(self.gate_rule(at=["place", "accept"]))
        self.start("o-5")
        self.set_gate(False, reason="closed for new work only")
        self.assertEqual(self.pull()["run"], [])
        self.assertEqual(self.running()["o-5"]["state"], "running")

    def test_a_bad_on_closed_is_refused(self):
        self.office_with(self.gate_rule(on_closed="whenever"))
        r = self.office("describe", self.REVIEWS)
        self.assertEqual(r.returncode, 1)
        self.assertIn("on_closed \"whenever\"", r.stderr)

    def test_a_surface_that_cant_be_told_still_holds(self):
        super(Run, self).write_machine(self.gate_plugins())  # no hold/resume/stop commands
        self.office_with(self.gate_rule())
        self.start("o-6")
        self.set_gate(False, reason="closed")
        self.pull()
        held = self.events(self.REVIEWS, "held")[0]
        self.assertFalse(held["told"])
        self.assertIn("has no hold command", held["why"])
        self.assertEqual(held["alert"]["level"], "smoke")
        self.assertEqual(self.running()["o-6"]["state"], "held")


class OwnerOverrides(Run):
    def test_close_now_interrupts_at_once_keeps_the_work_and_is_the_owners_stop(self):
        self.office_with(self.gate_rule(on_closed="finish-turn"))
        self.start("w-1")
        self.office_ok("close", self.REVIEWS, "--now")
        self.assertTrue(self.events(self.REVIEWS, "state")[-1]["now"])
        rep = self.pull()
        self.assertEqual(rep["run"][0]["on_closed"], "now")
        held = self.events(self.REVIEWS, "held")[0]
        self.assertEqual((held["by"], held["owners_stop"]), ("owner", True))
        self.assertEqual(self.surface_calls("hold")[0][2], "now")
        self.office_ok("open", self.REVIEWS)
        self.assertEqual(self.pull()["run"][0]["action"], "resumed")

    def test_close_until_beats_the_rules_and_expires_on_time(self):
        self.office_with(self.gate_rule())
        self.start("w-2")
        self.office_ok("close", self.REVIEWS, "--until", "2s")
        self.assertEqual(self.pull()["run"][0]["action"], "held")
        time.sleep(2.2)
        rep = self.pull()
        self.assertEqual(rep["run"][0]["action"], "resumed")
        exp = self.events(self.REVIEWS, "state-expired")
        self.assertEqual(len(exp), 1)
        self.assertEqual(exp[0]["state"], "closed")
        self.pull()
        self.assertEqual(len(self.events(self.REVIEWS, "state-expired")), 1)  # once

    def test_every_flip_says_what_it_replaced_and_owner_decisions_are_marked(self):
        self.office_with(self.gate_rule())
        self.office_ok("close", self.REVIEWS, "--kinds", "review")
        self.office_ok("open", self.REVIEWS)
        self.office_ok("clear", self.REVIEWS)
        flips = self.events(self.REVIEWS, "state")
        self.assertEqual(flips[0]["was"], "rules")
        self.assertEqual((flips[1]["was"]["state"], flips[1]["was"]["kinds"]), ("closed", ["review"]))
        self.assertEqual(flips[2]["was"]["state"], "open")
        self.set_gate(False, reason="gate shut")
        self.office_ok("open", self.REVIEWS)
        self.probe(self.REVIEWS, self.job())
        ev = self.events(self.REVIEWS, "evaluated")[-1]
        self.assertEqual((ev["result"], ev["by"], ev["override"]["state"]), ("open", "owner", "open"))

    def test_the_owner_stops_a_held_job_past_its_expiry(self):
        self.office_with(self.gate_rule())
        soon = "3s"
        self.place("--kind", "review", "--id", "w-3", "--expires", soon, code=0)
        self.pull()
        self.set_gate(False, reason="closed")
        self.pull()
        time.sleep(3.2)
        # Expires bounds starting, not running: held past it, it stays held.
        self.assertEqual(self.pull()["run"], [])
        self.assertEqual(self.running()["w-3"]["state"], "held")
        out = self.bowery("office", "stop", self.REVIEWS, "w-3")
        self.assertEqual(out.returncode, 0, out.stderr)
        self.assertIn("asked to stop", out.stdout)
        rep = self.pull()
        self.assertEqual(rep["run"][0]["action"], "stopped")
        self.assertEqual(self.surface_calls("stop")[0][0], "w-3")
        stopped = self.events(self.REVIEWS, "stopped")[0]
        self.assertEqual((stopped["by"], stopped["was"]), ("owner", "held"))
        self.assertEqual(self.running(), {})

    def test_a_held_job_past_expiry_resumes_when_the_office_opens(self):
        self.office_with(self.gate_rule())
        soon = "2s"
        self.place("--kind", "review", "--id", "w-4", "--expires", soon, code=0)
        self.pull()
        self.set_gate(False, reason="closed")
        self.pull()
        time.sleep(2.2)
        self.set_gate(True)
        self.assertEqual(self.pull()["run"], [{"id": "w-4", "action": "resumed"}])


class Collapse(Run):
    def test_repeated_run_phase_verdicts_collapse_into_one_line(self):
        self.office_with(self.gate_rule())
        self.start("c-1")
        for _ in range(4):
            self.pull()
        run = [e for e in self.events(self.REVIEWS, "verdict") if e["phase"] == "run"]
        self.assertEqual(len(run), 1)  # the first, logged when it came
        self.set_gate(False, reason="closed now")
        self.pull()
        run = [e for e in self.events(self.REVIEWS, "verdict") if e["phase"] == "run"]
        self.assertEqual([(e["verdict"], e.get("count")) for e in run], [("open", None), ("open", 3), ("closed", None)])
        self.assertTrue(run[1]["collapsed"])
        self.assertLessEqual(run[1]["first"], run[1]["last"])
        # A job that ends settles its runs.
        self.pull()
        self.pull()
        self.office_ok("done", self.REVIEWS, "c-1")
        run = [e for e in self.events(self.REVIEWS, "verdict") if e["phase"] == "run"]
        self.assertEqual(run[-1]["count"], 2)


class Facts(Run):
    def factful_rule(self):
        self.seen = self.t / "seen.jsonl"
        return textwrap.dedent(f"""
            [[rules.use]]
            name = "factful"
            plugin = "factful"
            [rules.use.config]
            seen = "{self.seen}"
            file = "{self.flag}"
            """)

    def requests(self):
        return [json.loads(l) for l in self.seen.read_text().splitlines()]

    def test_a_rule_gets_only_the_facts_it_declared_from_providers_and_gates(self):
        self.office_with(self.factful_rule())
        self.probe(self.REVIEWS, self.job())
        facts = self.requests()[-1]["machine"]["facts"]
        self.assertEqual(set(facts), {"load", "idle_s"})  # not mem_available_gb (undeclared), not absent
        self.assertEqual(facts["load"]["value"], 0.5)
        self.assertEqual(facts["idle_s"]["value"], 900)
        self.assertTrue(facts["load"]["at"].endswith("Z"))

    def test_a_failed_provider_or_a_stale_gate_is_missing_and_logged(self):
        self.office_with(self.factful_rule())
        (self.lib / "provider.mode").write_text("crash")
        self.set_gate(True, age=120)
        self.probe(self.REVIEWS, self.job())
        self.assertEqual(self.requests()[-1]["machine"]["facts"], {})
        ff = self.events(self.REVIEWS, "fact-failed")[0]
        self.assertEqual((ff["provider"], ff["alert"]["level"]), ("probe", "smoke"))
        (self.lib / "provider.mode").write_text("extra")
        self.probe(self.REVIEWS, self.job())
        self.assertIn("doesn't declare", self.events(self.REVIEWS, "fact-failed")[-1]["what"])

    def test_running_work_is_in_the_request(self):
        self.office_with(self.factful_rule())
        self.place("--kind", "review", "--id", "f-1", code=0)
        self.pull()
        self.probe(self.REVIEWS, self.job(id="f-2"))
        req = self.requests()[-1]
        self.assertEqual([(r["id"], r["held"]) for r in req["office"]["running"]], [("f-1", False)])
        self.assertEqual(req["machine"]["running"], [{"office": self.REVIEWS, "id": "f-1", "priority": ""}])

    def test_run_phase_requests_judge_the_running_job(self):
        self.office_with(self.factful_rule())
        self.place("--kind", "review", "--id", "f-3", code=0)
        self.pull()
        self.pull()
        run = [r for r in self.requests() if r["phase"] == "run"]
        self.assertEqual((run[-1]["job"]["id"], run[-1]["component"]), ("f-3", "run"))

    def test_check_finds_a_fact_nothing_supplies_and_two_sources_are_refused(self):
        self.office_with(self.factful_rule())
        r = self.office("check", self.REVIEWS)
        self.assertEqual(r.returncode, 1)
        self.assertIn("reads fact absent, which nothing on this machine supplies", r.stderr)
        self.write_machine(self.gate_plugins().replace('facts    = ["load"]', 'facts    = ["load", "idle_s"]'))
        r = self.office("describe", self.REVIEWS)
        self.assertEqual(r.returncode, 1)
        self.assertIn("fact idle_s is supplied by both", r.stderr)


@unittest.skipUnless(sys.platform.startswith("linux"), "the directory transport blocks on inotify on Linux")
class LongPoll(Run):
    def test_a_letter_is_taken_at_once_with_a_long_interval(self):
        # With a 30s look interval a polling loop would sit on the letter;
        # the long-poll wakes on it.
        loop = subprocess.Popen([OFFICE_BIN, "pull", self.REVIEWS, "--loop", "--interval", "30s", "--backstop", "60s",
                                 "--for", "30s"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            time.sleep(0.5)
            self.place("--kind", "review", "--id", "lp-1", code=0)
            deadline = time.monotonic() + 20  # under the loop's --for and the timer it rules out; generous for a loaded runner (CI-FAST)
            # The agent starts before the office logs its launch: wait for the line.
            launched = []
            while time.monotonic() < deadline and not launched:
                launched = self.events(self.REVIEWS, "launched")
                time.sleep(0.05)
            self.assertEqual(self.started_ids(), ["lp-1"])
            st = launched[0]["stages"]
            sent = datetime.fromisoformat(st["send"].replace("Z", "+00:00"))
            delivered = datetime.fromisoformat(st["delivered"].replace("Z", "+00:00"))
            # Taken on the letter's event, not at the 30s interval: well under it, generously (CI-FAST).
            self.assertLess((delivered - sent).total_seconds(), 10)
        finally:
            loop.terminate()
            loop.wait(5)
            loop.stdout.close()
            loop.stderr.close()

    def test_the_run_phase_runs_every_interval_under_the_loop(self):
        self.office_with(self.gate_rule(), interval="300ms")
        self.start("lp-2")
        # Fresh for one more second by its own clock. Nothing writes it
        # again, so no event comes: only the run interval can see it go
        # stale.
        self.set_gate(True, age=0, at_age=59)
        loop = subprocess.Popen([OFFICE_BIN, "pull", self.REVIEWS, "--loop", "--interval", "30s", "--backstop", "60s",
                                 "--for", "30s"], env=self.env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 20  # under the loop's --for and the timer it rules out; generous for a loaded runner (CI-FAST)
            while time.monotonic() < deadline and not self.surface_calls("hold"):
                time.sleep(0.05)
            self.assertEqual(self.surface_calls("hold")[0][0], "lp-2")
        finally:
            loop.terminate()
            loop.wait(5)
            loop.stdout.close()
            loop.stderr.close()


if __name__ == "__main__":
    unittest.main()
