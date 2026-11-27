"""The office bus's local node, in dev mode: `office bus serve` on a unix
socket under the region's var directory, and `office bus publish`, `follow`
and `status` as a region's own seats.

The node signs each event as the seat it saw at the socket and seals it as
the region; a seat publishes and reads only the topics the region file's
[bus.seats] give it. In dev mode the socket's peer is always the invoking
user, so OFFICE_BUS_AS names the seat instead (refused in root mode).

    python3 -m unittest discover tests
"""

import json
import os
import select
import sys
import subprocess
import textwrap
import time
import unittest

import _keys
from _binary import OFFICE_BIN
from test_offices import HOST, Offices

BUS = textwrap.dedent("""
    [bus]
    private = ["private.*"]

    [bus.seats.alice]
    publish = ["job.*", "private.*"]
    read    = ["*"]

    [bus.seats.bob]
    read = ["job.done"]
    """)


class BusNode(Offices):
    """A region with a bus node: the helpers, no tests of its own."""

    def setUp(self):
        super().setUp()
        self.write_region(BUS)
        self.write_machine()
        self.sock = self.var / "bus" / "node.sock"

    def write_machine(self, extra="", region_key=True):
        super().write_machine(extra)
        if region_key:
            p = self.etc / "machine.toml"
            p.write_text(p.read_text().replace(
                f'machine = "{HOST}"\n', f'machine = "{HOST}"\nregion_key = "{_keys.key("region-a")}"\n', 1))

    def serve(self, code=None):
        p = subprocess.Popen([OFFICE_BIN, "bus", "serve"], env=self.env,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

        def stop():
            if p.poll() is None:
                p.terminate()
            p.communicate(timeout=10)
        self.addCleanup(stop)
        line = p.stdout.readline()
        if code is not None:
            p.wait(timeout=10)
            self.assertEqual(p.returncode, code, line)
            return p
        self.assertIn(f"listening on {self.sock}", line, p.stderr.read() if p.poll() is not None else "")
        return p

    def bus(self, *args, seat="alice", **kw):
        env = {"OFFICE_BUS_AS": seat} if seat else {}
        return self.office("bus", *args, env=env, **kw)

    def bus_ok(self, *args, **kw):
        r = self.bus(*args, **kw)
        self.assertEqual(r.returncode, 0, r.stderr)
        return r.stdout

    def records(self, out):
        return [json.loads(l) for l in out.splitlines() if l.strip()]



@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class Bus(BusNode):
    def test_publish_and_follow(self):
        self.serve()
        out = self.bus_ok("publish", "job.started", "--owner", "alice", "--subject", '{"job":"j-1"}', "--json")
        rec = json.loads(out)
        ev = rec["event"]
        self.assertEqual((ev["producer"], ev["seq"], ev["region"], ev["kind"]), ("region-a/alice", 1, "region-a", "job.started"))
        self.assertEqual(ev["owners"], ["alice"])
        self.assertEqual(ev["subject"], {"job": "j-1"})
        self.assertIn("BEGIN SSH SIGNATURE", rec["sig"])
        self.assertIn("BEGIN SSH SIGNATURE", rec["seal"])
        self.assertEqual(self.bus_ok("publish", "job.done", "--cause", "region-a/alice:1").strip(), "region-a/alice:2")
        self.bus_ok("publish", "private.note", "--data", '{"on":"j-1","text":"t"}')

        got = self.records(self.bus_ok("follow", "--once"))
        self.assertEqual([r["event"]["seq"] for r in got], [1, 2, 3])
        self.assertEqual(got[1]["event"]["cause"], {"producer": "region-a/alice", "seq": 1})
        got = self.records(self.bus_ok("follow", "--once", "--cursor", "region-a/alice=1", "--topic", "job.*"))
        self.assertEqual([r["event"]["kind"] for r in got], ["job.done"])

        # bob reads job.done alone, and publishes nothing.
        got = self.records(self.bus_ok("follow", "--once", seat="bob"))
        self.assertEqual([r["event"]["kind"] for r in got], ["job.done"])
        r = self.bus("publish", "job.started", seat="bob")
        self.assertEqual(r.returncode, 1)
        self.assertIn("may not publish job.started", r.stderr)

        # The record is on disk, one line per event, in the producer's log.
        log = self.var / "bus" / "log" / "region-a+alice.log"
        self.assertEqual(len(log.read_text().splitlines()), 3)

    def test_status(self):
        self.serve()
        self.bus_ok("publish", "job.started")
        st = json.loads(self.bus_ok("status", "--json"))
        self.assertEqual(st["region"], "region-a")
        self.assertEqual(st["seat"], "alice")
        self.assertEqual(st["producer"], "region-a/alice")
        self.assertEqual(st["heads"], "region-a/alice=1")
        self.assertEqual(st["may_publish"], ["job.*", "private.*"])
        out = self.bus_ok("status", seat="bob")
        self.assertIn("seat      bob", out)
        self.assertIn("reads     job.done", out)
        self.assertIn("publishes -", out)

    def test_follow_wakes_at_once(self):
        self.serve()
        p = subprocess.Popen([OFFICE_BIN, "bus", "follow"], env={**self.env, "OFFICE_BUS_AS": "alice"},
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        self.addCleanup(lambda: (p.kill(), p.communicate(timeout=10)))
        time.sleep(0.3)
        for i in range(1, 4):
            start = time.monotonic()
            self.bus_ok("publish", "job.started")
            r, _, _ = select.select([p.stdout], [], [], 5)
            self.assertTrue(r, "no record within 5s")
            rec = json.loads(p.stdout.readline())
            self.assertEqual(rec["event"]["seq"], i)
            # The publish's own process takes most of this; the wake is inotify or in-process, not the 1s backstop.
            self.assertLess(time.monotonic() - start, 0.9)

    def test_as_and_bad_input(self):
        r = self.bus("status")
        self.assertEqual(r.returncode, 1)
        self.assertIn("no bus node", r.stderr)
        self.serve()
        for args, why in [
            (("publish", "job"), "isn't a topic"),
            (("publish", "job.started", "--subject", "{nope"), "--subject"),
            (("publish", "job.started", "--cause", "nope"), "--cause"),
            (("follow", "--cursor", "x"), "cursor"),
            (("follow", "--topic", "job*"), "job*"),
            (("publish",), "which topic"),
        ]:
            with self.subTest(args=args):
                r = self.bus(*args)
                self.assertEqual(r.returncode, 1, r.stdout)
                self.assertIn(why, r.stderr)
        r = self.bus("publish", "job.started", seat="carol")
        self.assertIn("may not publish", r.stderr)

    def test_one_node_and_its_keys(self):
        self.serve()
        p = self.serve(code=1)
        self.assertIn("another node", p.stderr.read())
        # Without the region key, there is nothing to seal with.
        self.write_machine(region_key=False)
        r = self.office("bus", "serve")
        self.assertEqual(r.returncode, 1)
        self.assertIn("region_key", r.stderr)
        # A region key that isn't the region's own is refused.
        p = self.etc / "machine.toml"
        p.write_text(p.read_text().replace(f'machine = "{HOST}"\n',
                                           f'machine = "{HOST}"\nregion_key = "{_keys.key("region-x")}"\n', 1))
        r = self.office("bus", "serve")
        self.assertEqual(r.returncode, 1)
        self.assertIn("isn't region region-a's key", r.stderr)

    def test_private_projection(self):
        """office bus publish --topic private.card checks the card schema;
        office private ls|show read the fold of the seat's private.*."""
        self.serve()
        out = self.bus_ok("publish", "--topic", "private.card", "--data",
                          '{"key":"watching","subject":"Things I\'m watching","refs":["key:shared-1"]}')
        self.assertEqual(out.strip(), "region-a/alice:1")
        self.bus_ok("publish", "--topic", "private.request", "--data", '{"key":"mine","title":"My grouping"}')
        self.bus_ok("publish", "private.card", "--data", '{"key":"watching","state":"done"}')
        # The schema is checked before anything reaches the node.
        for data, why in [('{"nope":1}', "nope"), ('{}', "subject"),
                          ('{"subject":"x","links":[{"url":"javascript:x"}]}', "http")]:
            with self.subTest(data=data):
                r = self.bus("publish", "--topic", "private.card", "--data", data)
                self.assertEqual(r.returncode, 1, r.stdout)
                self.assertIn(why, r.stderr)
        r = self.bus("publish", "private.thing", "--data", "{}")
        self.assertEqual(r.returncode, 1)
        self.assertIn("private.card", r.stderr)
        r = self.bus("publish", "job.started", "--topic", "job.done")
        self.assertEqual(r.returncode, 1)
        self.assertIn("one topic", r.stderr)
        # Shared never points at private.
        r = self.bus("publish", "job.started", "--data", '{"about":"private/watching"}')
        self.assertEqual(r.returncode, 1)
        self.assertIn("private", r.stderr)

        snap = json.loads(self.office_ok("private", "ls", "--json", env={"OFFICE_BUS_AS": "alice"}))
        self.assertEqual(snap["v"], 1)
        self.assertEqual(snap["cursor"], "region-a/alice=3")
        self.assertEqual(sorted(snap["lists"]), ["card", "hide", "note", "request", "ticket"])
        [card] = snap["lists"]["card"]
        self.assertEqual(card["key"], "private/watching")
        self.assertEqual((card["v"]["subject"], card["v"]["state"], card["v"]["private"]),
                         ("Things I'm watching", "done", True))
        # Replayed twice, the same bytes.
        self.assertEqual(self.office_ok("private", "ls", "--json", env={"OFFICE_BUS_AS": "alice"}),
                         self.office_ok("private", "ls", "--json", env={"OFFICE_BUS_AS": "alice"}))
        out = self.office_ok("private", "ls", env={"OFFICE_BUS_AS": "alice"})
        self.assertIn("private/watching", out)
        self.assertIn("Things I'm watching", out)
        self.assertIn("private/mine", out)
        out = self.office_ok("private", "ls", "--type", "request", env={"OFFICE_BUS_AS": "alice"})
        self.assertNotIn("private/watching", out)
        self.assertIn("private/mine", out)
        shown = json.loads(self.office_ok("private", "show", "watching", "--json", env={"OFFICE_BUS_AS": "alice"}))
        self.assertEqual((shown["type"], shown["key"], shown["v"]["refs"]), ("card", "private/watching", ["key:shared-1"]))
        r = self.office("private", "show", "private/nothing", env={"OFFICE_BUS_AS": "alice"})
        self.assertEqual(r.returncode, 1)
        self.assertIn("no private record", r.stderr)
        # bob reads no private.*: his fold is empty, not an error.
        snap = json.loads(self.office_ok("private", "ls", "--json", env={"OFFICE_BUS_AS": "bob"}))
        self.assertEqual(snap["lists"]["card"], [])

    def test_a_stale_socket_is_replaced(self):
        (self.var / "bus").mkdir(parents=True)
        self.sock.write_text("")
        self.serve()
        self.bus_ok("publish", "job.started")


FAILING_SURFACE = f"""#!{sys.executable}
import sys
sys.stdin.read()
print("starting the agent", file=sys.stderr)
print("refused: the conversation is still running elsewhere", file=sys.stderr)
sys.exit(2)
"""


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class LaunchFailed(BusNode):
    """A surface that exits non-zero leaves no running job, so nothing else
    would ever say so: pull publishes job.failed on the bus as the office's
    own seat (job, office, surface, exit, the surface's last stderr line)."""

    def setUp(self):
        super().setUp()
        self.write_region(BUS + f'\n[bus.seats.{self.REVIEWS}]\npublish = ["job.failed"]\n')

    def failing_surface(self):
        self.surface.write_text(FAILING_SURFACE)

    def failed(self):
        return [r["event"] for r in self.records(self.bus_ok("follow", "--once", "--topic", "job.failed"))]

    def test_a_launch_that_exits_non_zero_publishes_job_failed(self):
        self.serve()
        self.failing_surface()
        self.place("--kind", "review", "--id", "rev-fail", code=0)
        rep = self.pull()
        self.assertEqual(rep["accepted"][0]["exit"], 2)
        self.assertEqual(self.events(self.REVIEWS, "launched")[0]["exit"], 2)
        got = self.failed()
        self.assertEqual(len(got), 1, got)
        ev = got[0]
        self.assertEqual(ev["producer"], f"region-a/{self.REVIEWS}")
        self.assertEqual(ev["subject"], {"job": "rev-fail", "office": self.REVIEWS, "kind": "review"})
        self.assertEqual(ev["data"]["surface"], "reviewer")
        self.assertEqual(ev["data"]["exit"], 2)
        self.assertEqual(ev["data"]["stderr"], "refused: the conversation is still running elsewhere")
        # The office's log says it went out, and which record it is.
        pub = self.events(self.REVIEWS, "failed-published")
        self.assertEqual(pub[0]["job"], "rev-fail")
        self.assertEqual(pub[0]["ref"], f"region-a/{self.REVIEWS}:1")
        # Nothing is left running for a job that never started.
        self.assertEqual(self.pull()["accepted"], [])

    def test_a_launch_that_starts_publishes_nothing(self):
        self.serve()
        self.place("--kind", "review", "--id", "rev-ok", code=0)
        self.assertEqual(self.pull()["accepted"][0]["exit"], 0)
        self.assertEqual(self.failed(), [])
        self.assertEqual(self.events(self.REVIEWS, "failed-published"), [])

    def test_no_bus_node_is_logged_and_the_pull_goes_on(self):
        self.failing_surface()
        self.place("--kind", "review", "--id", "rev-nobus", code=0)
        self.assertEqual(self.pull()["accepted"][0]["exit"], 2)
        miss = self.events(self.REVIEWS, "failed-not-published")
        self.assertEqual(miss[0]["job"], "rev-nobus")
        self.assertIn("no bus node", miss[0]["why"])

    def test_a_seat_that_may_not_publish_is_logged(self):
        self.write_region(BUS)
        self.serve()
        self.failing_surface()
        self.place("--kind", "review", "--id", "rev-noseat", code=0)
        self.pull()
        miss = self.events(self.REVIEWS, "failed-not-published")
        self.assertIn("may not publish", miss[0]["why"])
        self.assertEqual(self.failed(), [])


if __name__ == "__main__":
    unittest.main()
