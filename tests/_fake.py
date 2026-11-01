"""The test bed: bowery against a fake herdr, a fake ssh and real git.

Every test gets a throwaway directory holding the fake herdr's servers, the
dispatch records, the worktrees, a config file, a git "origin" with two
clones of it (REPO, the main checkout agents branch from, and OTHER, which
plays GitHub merging a pull request), and a fake box for --machine: a home
directory reached through the fake ssh, with an identity for "alice".

fakes/ is put first on PATH, so `herdr` and `ssh` are the stand-ins; git is
the real one. HERDR_SOCKET_PATH is a socket served here, for the one herdr
method bowery calls over the socket (workspace.move).
"""

import fcntl
import json
import os
import shutil
import socket
import subprocess
import tempfile
import threading
import unittest
from contextlib import contextmanager
from pathlib import Path

from _binary import BIN, ROOT

FAKES = ROOT / "tests" / "fakes"
BOX = "swarm-box"


def sh(*args, cwd=None, check=True):
    return subprocess.run(args, cwd=cwd, check=check, capture_output=True, text=True).stdout.strip()


class Bed(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory(prefix="bowery-")
        self.t = t = Path(os.path.realpath(self._tmp.name))
        self.herdr_dir = t / "herdr"
        self.state = t / "state"
        self.wt = t / "worktrees"
        self.home = t / "home"
        self.box = t / "box"
        self.config = t / "config.toml"
        for d in (self.herdr_dir, self.home, self.box / "identities"):
            d.mkdir(parents=True)
        (self.box / "identities" / "alice.env").write_text("GIT_AUTHOR_NAME=alice\nGIT_COMMITTER_NAME=alice\n")
        self.env = {
            "PATH": f"{FAKES}:{os.environ['PATH']}",
            "HOME": str(self.home),
            "FAKE_HERDR_DIR": str(self.herdr_dir),
            "FAKE_SSH_HOSTS": f"{BOX}={self.box}",
            "HERDR_SOCKET_PATH": str(t / "herdr.sock"),
            "XDG_RUNTIME_DIR": str(t),
            "BOWERY_STATE": str(self.state),
            "BOWERY_WORKTREE_ROOT": str(self.wt),
            "BOWERY_CONFIG": str(self.config),
            "BOWERY_TRUST_ROOTS": str(t),
            "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t",
            "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t",
            "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
        }
        os.environ.update({k: v for k, v in self.env.items() if k.startswith("GIT_")})
        self.repo, self.other = self.make_repo("repo")
        self._serve_socket()

    def tearDown(self):
        self._server.close()
        self._tmp.cleanup()

    # ------------------------------------------------------------- git

    def make_repo(self, name, parent=None):
        """An origin with a main branch, its clone `name`, and a second clone
        (`name`-other) to merge pull requests in."""
        parent = Path(parent or self.t)
        origin = parent / f"{name}-origin.git"
        repo, other = parent / name, parent / f"{name}-other"
        sh("git", "init", "-q", "--bare", "-b", "main", str(origin))
        sh("git", "clone", "-q", str(origin), str(repo))
        (repo / "file").write_text("base\n")
        sh("git", "-C", str(repo), "add", "file")
        sh("git", "-C", str(repo), "commit", "-qm", "init")
        sh("git", "-C", str(repo), "push", "-q", "origin", "main")
        sh("git", "-C", str(repo), "remote", "set-head", "origin", "main")
        sh("git", "clone", "-q", str(origin), str(other))
        return repo, other

    def commit_in(self, path, name):
        (Path(path) / name).write_text(name + "\n")
        sh("git", "-C", str(path), "add", name)
        sh("git", "-C", str(path), "commit", "-qm", name)
        sh("git", "-C", str(path), "push", "-q", "origin", "HEAD")

    def merge_pr(self, branch, how="merge", other=None):
        o = str(other or self.other)
        sh("git", "-C", o, "fetch", "-q", "origin")
        sh("git", "-C", o, "checkout", "-q", "main")
        sh("git", "-C", o, "merge", "-q", "--ff-only", "origin/main")
        if how == "squash":
            sh("git", "-C", o, "merge", "-q", "--squash", f"origin/{branch}")
            sh("git", "-C", o, "commit", "-qm", f"squash {branch}")
        else:
            sh("git", "-C", o, "merge", "-q", "--no-ff", "-m", f"merge {branch}", f"origin/{branch}")
        sh("git", "-C", o, "push", "-q", "origin", "main")

    def branches(self, repo=None):
        return sh("git", "-C", str(repo or self.repo), "branch", "--format=%(refname:short)").split()

    # ------------------------------------------------------------- bowery

    def bowery(self, *args, env=None, cwd=None):
        """-> (exit status, stdout + stderr)."""
        p = subprocess.run([BIN, *map(str, args)], env={**self.env, **(env or {})}, cwd=cwd or self.t,
                           capture_output=True, text=True, stdin=subprocess.DEVNULL)
        return p.returncode, p.stdout + p.stderr

    def ok(self, *args, **kw):
        rc, out = self.bowery(*args, **kw)
        self.assertEqual(rc, 0, out)
        return out

    def refused(self, want, *args, **kw):
        rc, out = self.bowery(*args, **kw)
        self.assertNotEqual(rc, 0, out)
        self.assertIn(want, out)
        return out

    def dispatch(self, name, *args, **kw):
        out = self.ok("dispatch", name, *args, **kw)
        return json.loads(out.strip().splitlines()[-1])

    def records(self, name="*"):
        return sorted(self.state.glob(f"{name}.*.json"))

    def record(self, name):
        (f,) = self.records(name)
        return json.loads(f.read_text())

    # ------------------------------------------------------------- herdr

    def server(self, machine=None):
        return self.herdr_dir / (f"machine-{machine}.json" if machine else "local.json")

    def state_of(self, machine=None):
        try:
            return json.loads(self.server(machine).read_text())
        except FileNotFoundError:
            return {"workspaces": [], "tabs": {}, "panes": [], "agents": {}, "log": []}

    @contextmanager
    def mutate(self, machine=None):
        """Change the fake server's state, as the world would."""
        with open(self.herdr_dir / ".lock", "w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            s = self.state_of(machine)
            yield s
            self.server(machine).write_text(json.dumps(s))

    def herdr(self, *args, machine=None):
        pre = ["--machine", machine] if machine else []
        p = subprocess.run(["herdr", *pre, *map(str, args)], env={**os.environ, **self.env}, capture_output=True,
                           text=True)
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        return json.loads(p.stdout)["result"]

    def ws_labels(self, machine=None):
        return [w["label"] for w in self.state_of(machine)["workspaces"]]

    def swarm(self, machine=None):
        return next((w for w in self.state_of(machine)["workspaces"] if w["label"] == "swarm"), None)

    def tab_labels(self, wid, machine=None):
        s = self.state_of(machine)
        w = next(w for w in s["workspaces"] if w["workspace_id"] == wid)
        return [s["tabs"][t]["label"] for t in w["tabs"]]

    def agent(self, name, machine=None):
        return self.state_of(machine)["agents"].get(name)

    def agent_names(self, machine=None):
        return list(self.state_of(machine)["agents"])

    def pane(self, pid, machine=None):
        return next((p for p in self.state_of(machine)["panes"] if p["pane_id"] == pid), None)

    def terminals(self, machine=None):
        return [p["terminal_id"] for p in self.state_of(machine)["panes"]]

    def set_status(self, name, status, machine=None):
        with self.mutate(machine) as s:
            s["agents"][name]["agent_status"] = status

    def split_off(self, pid, machine=None):
        return self.herdr("pane", "split", "--pane", pid, "--direction", "down", "--no-focus", machine=machine)["pane"]

    def run_in(self, pid, cmd, machine=None):
        self.herdr("pane", "run", pid, cmd, machine=machine)

    def new_workspace(self, label, cwd):
        return self.herdr("workspace", "create", "--label", label, "--cwd", cwd, "--no-focus")["root_pane"]

    # ------------------------------------------------------------- the box

    def add_box(self, identity=True, extra=""):
        """Save the fake box as herdr machine `swarm-box`, with alice as an
        identity there (identity=False: a box without identities)."""
        (self.herdr_dir / "machines.json").write_text(json.dumps(
            [{"id": "0" * 32, "label": BOX, "target": BOX, "session": "default", "enabled": True}]))
        conf = f'[machines.{BOX}]\n'
        if identity:
            conf += f'identity_command = "{FAKES}/as"\n'
        self.config.write_text(conf + extra)

    def box_repo(self):
        """The app repo, cloned on the box at ~/dev/app."""
        (self.box / "dev").mkdir(exist_ok=True)
        return self.make_repo("app", self.box / "dev")

    def ssh_log(self):
        try:
            return (self.herdr_dir / "ssh.log").read_text()
        except FileNotFoundError:
            return ""

    # ------------------------------------------------------------- socket

    def _serve_socket(self):
        """workspace.move, the one raw socket method bowery uses."""
        path = self.env["HERDR_SOCKET_PATH"]
        self._server = srv = socket.socket(socket.AF_UNIX)
        srv.bind(path)
        srv.listen(4)
        self.moves = []

        def serve():
            while True:
                try:
                    conn, _ = srv.accept()
                except OSError:
                    return
                with conn:
                    buf = b""
                    while b"\n" not in buf:
                        chunk = conn.recv(65536)
                        if not chunk:
                            break
                        buf += chunk
                    req = json.loads(buf.split(b"\n")[0])
                    self.moves.append(req)
                    if req["method"] == "workspace.move":
                        p = req["params"]
                        with self.mutate() as s:
                            ws = s["workspaces"]
                            w = next(w for w in ws if w["workspace_id"] == p["workspace_id"])
                            i = ws.index(w)
                            ws.insert(p["insert_index"], w)
                            del ws[i if i < p["insert_index"] else i + 1]
                    conn.sendall((json.dumps({"id": req["id"], "result": {"type": "ok"}}) + "\n").encode())

        threading.Thread(target=serve, daemon=True).start()
