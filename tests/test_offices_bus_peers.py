"""The office bus between regions, in dev mode: two regions on one machine,
each running `office bus serve`, replicating over the office transport.

region-a lets region-b read job.* (and private.*, which never leaves
anyway) and publish card.*; region-b lets region-a publish job.* and read
card.*. Each node pushes what the peer may read and pulls what the peer may
publish, every call signed with its region key; each side checks the rules
again on arrival. Over the dir transport a peer's address is its var
directory (its node's socket is there); over http it's the URL of its
`office serve`, which hands the call to its node.

    python3 -m unittest discover tests
"""

import json
import re
import subprocess
import textwrap
import time
import unittest

import _keys
from _binary import OFFICE_BIN, SLOW_CI
from test_offices import HOST
import test_offices_bus as one_node
from test_offices_bus import BUS

A_PEER = """
[peers.region-b]
address     = "{addr}"
key         = "{key}"
may_place   = []
may_view    = []
may_publish = ["card.*"]
may_read    = ["job.*", "private.*"]
"""


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class Peers(one_node.Bus):
    def setUp(self):
        super().setUp()
        self.b_root = self.t / "region-b"
        (self.b_root / "etc").mkdir(parents=True)
        self.b_var = self.b_root / "var"
        self.b_env = {**self.env, "OFFICE_ROOT": str(self.b_root), "USER": "bob"}
        self.a_addr, self.b_addr = str(self.var), str(self.b_var)
        self.endpoints()
        self.write_both()

    def endpoints(self):
        pass

    def write_both(self, b_knows_a=True):
        self.write_region(BUS + A_PEER.format(addr=self.b_addr, key=_keys.pub("region-b")))
        b_peer = textwrap.dedent(f"""
            [peers.region-a]
            {'address     = "%s"' % self.a_addr if b_knows_a else ''}
            key         = "{_keys.pub('region-a')}"
            may_place   = []
            may_view    = []
            may_publish = ["job.*"]
            may_read    = ["card.*"]
            """)
        (self.b_root / "etc" / "region.toml").write_text(textwrap.dedent(f"""\
            region   = "region-b"
            owner    = "bob"
            timezone = "UTC"
            key      = "{_keys.pub('region-b')}"
            offices  = []
            priorities  = ["high", "low"]
            max_expires = "24h"

            [bus.seats.bob]
            publish = ["card.*", "gate.*"]
            read    = ["*"]
            """) + b_peer)
        (self.b_root / "etc" / "machine.toml").write_text(
            f'machine = "{HOST}"\nregion_key = "{_keys.key("region-b")}"\n' + self.b_machine_extra())

    def b_machine_extra(self):
        return ""

    def serve_b(self):
        p = subprocess.Popen([OFFICE_BIN, "bus", "serve"], env=self.b_env,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

        def stop():
            if p.poll() is None:
                p.terminate()
            p.communicate(timeout=10)
        self.addCleanup(stop)
        line = p.stdout.readline()
        self.assertIn("listening on", line, p.stderr.read() if p.poll() is not None else "")
        return p

    def b_bus(self, *args):
        r = subprocess.run([OFFICE_BIN, "bus", *args], env={**self.b_env, "OFFICE_BUS_AS": "bob"},
                           capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 0, r.stderr)
        return r.stdout

    def kinds(self, out, producer):
        return [r["event"]["kind"] for r in self.records(out) if r["event"]["producer"] == producer]

    # A failed call is retried from 1s doubling to 30s, so a peering that
    # failed while its peer came up (slowly, on a loaded runner) may next try
    # half a minute later: the default bound covers that ceiling and a call.
    # Latency claims bound their own (a few seconds) where they're made.
    def until(self, f, want, within=40):
        deadline = time.monotonic() + within
        got = None
        while time.monotonic() < deadline:
            got = f()
            if got == want:
                return time.monotonic()
            time.sleep(0.05)
        status = self.office("bus", "status", env={"OFFICE_BUS_AS": "alice"})
        self.fail(f"got {got!r}, want {want!r}; region-a's bus status:\n{status.stdout}{status.stderr}")

    def a_has(self):
        return self.kinds(self.bus_ok("follow", "--once"), "region-b/bob")

    def b_has(self):
        return self.kinds(self.b_bus("follow", "--once"), "region-a/alice")

    # ------------------------------------------------------------ tests

    def test_events_cross_both_ways_within_the_rules(self):
        self.serve()
        self.serve_b()
        start = time.monotonic()
        self.bus_ok("publish", "job.started")
        self.bus_ok("publish", "private.note")
        self.b_bus("publish", "card.answered")
        self.b_bus("publish", "gate.decided")  # region-a doesn't let region-b read it in
        # Neither way waits on a timer: a push follows the publish, a pull
        # long-polls the peer.
        b_at, a_at = self.until(self.b_has, ["job.started"]), self.until(self.a_has, ["card.answered"])
        if not SLOW_CI:  # timing only; see SLOW_CI in _binary.py (CI-FAST)
            self.assertLess(b_at - start, 3)
            self.assertLess(a_at - start, 3)
        # What each kept verifies as the other's: north's seat, north's seal.
        rec = [r for r in self.records(self.b_bus("follow", "--once")) if r["event"]["producer"] == "region-a/alice"][0]
        self.assertEqual(rec["event"]["region"], "region-a")
        self.assertIn("BEGIN SSH SIGNATURE", rec["seal"])

        # The cursors are kept per peer (region-b came up after region-a's
        # first pull failed: its retry is a second away).
        def peer():
            st = json.loads(self.bus_ok("status", "--json"))
            p = [p for p in st["peers"] if p["peer"] == "region-b"][0]
            return "region-a/alice=2" in p["pushed"] and "region-b/bob=2" in p["pulled"] and not p.get("error")
        self.until(peer, True)
        self.assertIn("region-b: pushed region-a/alice=2", self.bus_ok("status"))

    def test_one_side_reaching_the_other_does_both(self):
        # region-b has no address for region-a (a machine behind a
        # firewall, seen from the other side): region-a pushes and pulls.
        self.write_both(b_knows_a=False)
        self.serve()
        self.serve_b()
        self.bus_ok("publish", "job.started")
        self.b_bus("publish", "card.answered")
        self.until(self.b_has, ["job.started"])
        self.until(self.a_has, ["card.answered"])
        st = json.loads(self.b_bus("status", "--json"))
        self.assertEqual(st["peers"], [])

    def test_a_peer_that_was_down_gets_what_it_missed(self):
        self.serve()
        self.bus_ok("publish", "job.started")
        time.sleep(0.5)  # region-a tries, and fails: region-b isn't up
        self.bus_ok("publish", "job.done")
        self.serve_b()
        self.until(self.b_has, ["job.started", "job.done"], within=40)

    def test_a_stranger_is_refused(self):
        # region-b's file pins another key for region-a: its calls are
        # refused, and nothing crosses either way.
        self.write_both(b_knows_a=False)
        p = self.b_root / "etc" / "region.toml"
        p.write_text(p.read_text().replace(_keys.pub("region-a"), _keys.pub("mallory")))
        self.serve()
        self.serve_b()
        self.bus_ok("publish", "job.started")
        self.b_bus("publish", "card.answered")

        def error():
            st = json.loads(self.bus_ok("status", "--json"))
            peer = [p for p in st["peers"] if p["peer"] == "region-b"]
            return bool(peer) and "isn't region-a's key" in peer[0].get("error", "")
        self.until(error, True)
        self.assertEqual(self.b_has(), [])
        self.assertEqual(self.a_has(), [])


# The one-node tests run in test_offices_bus; these classes borrow its setup.
for _name in [n for n in dir(one_node.Bus) if n.startswith("test")]:
    setattr(Peers, _name, None)


@unittest.skipUnless(_keys.have_ssh_keygen(), "needs ssh-keygen")
class PeersOverHTTP(Peers):
    """The same, with each region's `office serve` as its address."""

    def endpoints(self):
        self.a_addr = self.office_serve(self.env)
        self.b_addr = self.office_serve(self.b_env)

    def write_machine(self, extra="", region_key=True):
        super().write_machine(extra, region_key)
        p = self.etc / "machine.toml"
        p.write_text(p.read_text().replace(f'machine = "{HOST}"\n', f'machine = "{HOST}"\ntransport = "http"\n', 1))

    def b_machine_extra(self):
        return 'transport = "http"\n'

    def office_serve(self, env):
        # Its region and machine files are there before it starts.
        if env is self.b_env:
            (self.b_root / "etc" / "region.toml").write_text(
                f'region = "region-b"\nowner = "bob"\ntimezone = "UTC"\nkey = "{_keys.pub("region-b")}"\n'
                'offices = []\npriorities = ["high", "low"]\nmax_expires = "24h"\n')
            (self.b_root / "etc" / "machine.toml").write_text(f'machine = "{HOST}"\ntransport = "http"\n')
        p = subprocess.Popen([OFFICE_BIN, "serve", "--listen", "127.0.0.1:0"], env=env,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

        def stop():
            p.terminate()
            p.communicate(timeout=10)
        self.addCleanup(stop)
        line = p.stdout.readline()
        m = re.match(r"listening on (http://\S+)", line)
        self.assertTrue(m, line or p.stderr.read())
        return m.group(1)

    def test_the_endpoint_answers_only_a_signed_call(self):
        import urllib.error
        import urllib.request
        self.serve()
        req = urllib.request.Request(self.a_addr + "/bus/peer", data=b'{"call":{"op":"pull"},"sig":"x"}', method="POST")
        req.add_header("Content-Type", "application/json")
        with urllib.request.urlopen(req, timeout=10) as r:
            rep = json.loads(r.read())
        self.assertIn("isn't a peer", rep["error"])
        with self.assertRaises(urllib.error.HTTPError) as e:
            urllib.request.urlopen(urllib.request.Request(self.a_addr + "/bus/peer"), timeout=10)
        self.assertEqual(e.exception.code, 405)
        e.exception.close()


if __name__ == "__main__":
    unittest.main()
