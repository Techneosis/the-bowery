"""The bus node's private web endpoint, end to end: `office bus serve` with
a [bus.web] serves the private projection over HTTPS to the owner's
browser, its certificate and its callers' identities from a fake tailscaled
(the LocalAPI on a Unix socket), a self-signed certificate standing in for
the tailnet's.

    python3 -m unittest discover tests
"""

import http.client
import http.server
import json
import os
import shutil
import socket
import socketserver
import ssl
import subprocess
import tempfile
import textwrap
import threading
import time
import unittest
import urllib.parse

import _keys
import test_offices_bus as tob
from test_offices import Offices

BUS = tob.BUS

HOST = "node.example.ts.net"
OWNER = "alice@example.com"
ORIGIN = "https://board.example.ts.net"


def self_signed(d):
    """A key and a certificate for HOST, in tailscaled's "pair" format."""
    key, crt = os.path.join(d, "k.pem"), os.path.join(d, "c.pem")
    r = subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", key, "-out", crt,
                        "-days", "90", "-subj", "/CN=" + HOST, "-addext", "subjectAltName=DNS:" + HOST],
                       capture_output=True, text=True)
    if r.returncode != 0:
        return None, None
    return open(key).read() + open(crt).read(), crt


def have_openssl():
    if not shutil.which("openssl"):
        return False
    d = tempfile.mkdtemp()
    try:
        return self_signed(d)[0] is not None
    finally:
        shutil.rmtree(d, ignore_errors=True)


class FakeTailscaled(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def __init__(self, path, pair):
        self.pair = pair
        self.login = OWNER
        self.whois_calls = 0

        class H(http.server.BaseHTTPRequestHandler):
            def address_string(self):
                return "unix"

            def log_message(self, *a):
                pass

            def do_GET(h):
                u = urllib.parse.urlparse(h.path)
                q = urllib.parse.parse_qs(u.query)
                if u.path == "/localapi/v0/whois":
                    self.whois_calls += 1
                    body = json.dumps({"Node": {"Tags": None}, "UserProfile": {"LoginName": self.login}}).encode()
                elif u.path == "/localapi/v0/cert/" + HOST and q.get("type") == ["pair"]:
                    body = self.pair.encode()
                else:
                    h.send_error(404)
                    return
                h.send_response(200)
                h.send_header("Content-Length", str(len(body)))
                h.end_headers()
                h.wfile.write(body)

        super().__init__(path, H)


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


@unittest.skipUnless(_keys.have_ssh_keygen() and have_openssl(), "needs ssh-keygen and openssl")
class BusWeb(Offices):
    # The bus tests' helpers, without their tests.
    def write_machine(self, extra=""):
        Offices.write_machine(self, extra)
        p = self.etc / "machine.toml"
        p.write_text(p.read_text().replace(
            f'machine = "{tob.HOST}"\n', f'machine = "{tob.HOST}"\nregion_key = "{_keys.key("region-a")}"\n', 1))

    serve = tob.Bus.serve
    bus = tob.Bus.bus
    bus_ok = tob.Bus.bus_ok

    def setUp(self):
        super().setUp()
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, ignore_errors=True)
        pair, self.crt = self_signed(d)
        ts = os.path.join(d, "ts.sock")
        self.ts = FakeTailscaled(ts, pair)
        threading.Thread(target=self.ts.serve_forever, daemon=True).start()
        self.addCleanup(self.ts.server_close)
        self.addCleanup(self.ts.shutdown)
        self.port = free_port()
        self.write_region(BUS + textwrap.dedent(f"""
            [bus.web]
            listen     = "127.0.0.1:{self.port}"
            host       = "{HOST}"
            owner      = "{OWNER}"
            origin     = "{ORIGIN}"
            seat       = "alice"
            tailscaled = "{ts}"
            """))
        self.write_machine()
        self.sock = self.var / "bus" / "node.sock"

    def conn(self, timeout=10):
        ctx = ssl.create_default_context(cafile=self.crt)
        c = http.client.HTTPSConnection(HOST, self.port, context=ctx, timeout=timeout)
        c.sock = ctx.wrap_socket(socket.create_connection(("127.0.0.1", self.port), timeout=timeout), server_hostname=HOST)
        return c

    def ask(self, method, path, origin=ORIGIN, body=None, headers=None):
        c = self.conn()
        h = dict(headers or {})
        if origin:
            h["Origin"] = origin
        if body is not None:
            h["Content-Type"] = "application/json"
            body = json.dumps(body)
        c.request(method, path, body=body, headers=h)
        r = c.getresponse()
        out = r.status, dict(r.getheaders()), r.read().decode()
        c.close()
        return out

    def up(self):
        """Serve, and wait until the endpoint answers over TLS."""
        p = self.serve()
        deadline = time.monotonic() + 15
        while True:
            try:
                self.conn(timeout=2).close()
                return p
            except (OSError, ssl.SSLError):
                if time.monotonic() > deadline or p.poll() is not None:
                    raise
                time.sleep(0.1)

    def test_the_owner_reads_and_writes_private_records(self):
        self.up()
        self.bus_ok("publish", "private.card", "--data", '{"key":"watching","subject":"Things I watch"}')
        self.bus_ok("publish", "job.started", "--subject", '{"job":"j-1"}')

        st, hdr, body = self.ask("GET", "/private/snapshot")
        self.assertEqual(st, 200, body)
        self.assertEqual(hdr.get("Access-Control-Allow-Origin"), ORIGIN)
        snap = json.loads(body)
        self.assertEqual([e["key"] for e in snap["snapshot"]["lists"]["card"]], ["private/watching"])
        self.assertEqual(snap["snapshot"]["cursor"], "region-a/alice=2")
        self.assertNotIn("j-1", body)

        card = {"kind": "private.card", "data": {"key": "later", "subject": "Later"}}
        st, _, body = self.ask("POST", "/private/publish", body=card)
        self.assertEqual(st, 403, body)  # no token
        st, _, body = self.ask("POST", "/private/publish", body=card, headers={"X-CSRF-Token": snap["csrf"]})
        self.assertEqual(st, 200, body)
        self.assertEqual(json.loads(body)["ref"], "region-a/alice:3")
        # It's an event in the node's log like any other, signed as the seat.
        out = self.bus_ok("follow", "--once", "--topic", "private.*")
        self.assertEqual([json.loads(l)["event"]["seq"] for l in out.splitlines()], [1, 3])

        st, _, _ = self.ask("GET", "/private/snapshot", origin="https://evil.example")
        self.assertEqual(st, 403)

    def test_anyone_but_the_owner_is_refused(self):
        self.ts.login = "bob@example.com"
        self.up()
        st, hdr, body = self.ask("GET", "/private/snapshot")
        self.assertEqual(st, 403, body)
        self.assertNotIn("Access-Control-Allow-Origin", hdr)
        self.ts.login = OWNER
        st, _, body = self.ask("GET", "/private/snapshot")
        self.assertEqual(st, 200, body)

    def test_the_stream_wakes_on_a_publish(self):
        self.up()
        c = self.conn()
        c.request("GET", "/private/stream", headers={"Origin": ORIGIN})
        r = c.getresponse()
        self.assertEqual(r.status, 200)
        self.assertTrue(r.getheader("Content-Type").startswith("text/event-stream"))

        def event():
            ev = {}
            while True:
                line = r.fp.readline().decode().rstrip("\n")
                if line == "" and "event" in ev:
                    return ev
                if line.startswith("event: "):
                    ev["event"] = line[7:]
                elif line.startswith("data: "):
                    ev["data"] = json.loads(line[6:])

        self.assertEqual(event()["event"], "snapshot")
        start = time.monotonic()
        self.bus_ok("publish", "private.ticket", "--data", '{"key":"t1","title":"T"}')
        ev = event()
        took = time.monotonic() - start
        print(f"\n  a private publish reached the browser's stream in {took * 1000:.0f} ms", end=" ")
        self.assertEqual(ev["event"], "change")
        self.assertEqual([e["key"] for e in ev["data"]["put"]["ticket"]], ["private/t1"])
        c.close()


if __name__ == "__main__":
    unittest.main()
