"""scripts/ci-local's verdict: it exits 0 only when every side passed.

The suite itself is faked (a commit whose scripts/test-suite just prints a
line, `go` and `gofmt` stubs), and so is the other machine: an `ssh` on PATH
that runs git's own pack commands locally (so the push works) and answers the
suite's command with the exit the test picks, after a delay, so this side has
finished first.
"""
import os
import pathlib
import stat
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent

FAKE_SSH = """#!/bin/sh
# Skip ssh's options and the host; what's left is the remote command.
while [ $# -gt 0 ]; do
  case $1 in
    -o|-p|-l|-i) shift 2 ;;
    -*) shift ;;
    *) shift; break ;;
  esac
done
case "$*" in
  git-*) exec sh -c "$*" ;;
esac
sleep "${FAKE_SSH_DELAY:-2}"
echo "test-suite: ran 1 tests in 1 processes, 0s: fake there"
exit "${FAKE_SSH_EXIT:-0}"
"""

FAKE_GO = """#!/bin/sh
case $1 in env) echo fake ;; esac
exit 0
"""

FAKE_SUITE = """#!/bin/sh
echo "test-suite: ran 1 tests in 1 processes, 0s: OK"
"""


def write_exe(path, text):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


def bash_has_wait_p():
    """ci-local needs bash 5.1's `wait -n -p`; the bash on PATH may be older
    (macOS ships 3.2, and so do GitHub's macOS runners)."""
    try:
        return subprocess.run(["bash", "-c", "sleep 0 & wait -n -p p $!"], capture_output=True,
                              timeout=10).returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


@unittest.skipUnless(bash_has_wait_p(), "the bash on PATH has no `wait -n -p` (needs 5.1+), which ci-local needs")
class CiLocal(unittest.TestCase):
    def setUp(self):
        self.tmp = pathlib.Path(tempfile.mkdtemp(prefix="ci-local-test."))
        self.repo = self.tmp / "repo"
        bin_ = self.tmp / "bin"
        write_exe(bin_ / "ssh", FAKE_SSH)
        write_exe(bin_ / "go", FAKE_GO)
        write_exe(bin_ / "gofmt", "#!/bin/sh\nexit 0\n")
        write_exe(self.repo / "scripts" / "ci-local", (ROOT / "scripts" / "ci-local").read_text())
        write_exe(self.repo / "scripts" / "test-suite", FAKE_SUITE)
        self.env = {**os.environ, "PATH": f"{bin_}{os.pathsep}{os.environ['PATH']}",
                    "TMPDIR": str(self.tmp), "BOWERY_CI_REMOTE": "fakehost",
                    "BOWERY_CI_REMOTE_DIR": str(self.repo),
                    "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
                    "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com"}
        for argv in (["init", "-q"], ["add", "-A"], ["commit", "-q", "-m", "fake suite"]):
            subprocess.run(["git", *argv], cwd=self.repo, env=self.env, check=True)

    def run_ci(self, there_exit):
        return subprocess.run([str(self.repo / "scripts" / "ci-local")], cwd=self.repo,
                              env={**self.env, "FAKE_SSH_EXIT": str(there_exit)},
                              capture_output=True, text=True, timeout=60)

    def test_passes_when_both_sides_pass(self):
        r = self.run_ci(0)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("here passed", r.stdout)
        self.assertIn("there passed", r.stdout)

    def test_fails_when_the_other_side_fails_after_this_one_passed(self):
        # bash 5.3's `wait -n` hands back a job it already reported, with its
        # status, when its pid is still in the list: the slower side's
        # failure was never waited for, and ci-local said "here passed" twice.
        r = self.run_ci(1)
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn("here passed", r.stdout)
        self.assertIn("there FAILED", r.stdout)
        self.assertEqual(r.stdout.count("here passed"), 1, r.stdout)


if __name__ == "__main__":
    unittest.main()
