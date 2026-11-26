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


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class Bus(Offices):
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

    # ------------------------------------------------------------ tests

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
        self.bus_ok("publish", "private.note", "--data", '{"n":1}')

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

    def test_a_stale_socket_is_replaced(self):
        (self.var / "bus").mkdir(parents=True)
        self.sock.write_text("")
        self.serve()
        self.bus_ok("publish", "job.started")


if __name__ == "__main__":
    unittest.main()
