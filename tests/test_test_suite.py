"""scripts/test-suite's verdict on a shard: what unittest said, wherever it is.

A test may print a summary line to stdout (a timing, say). stdout is flushed
when the shard's process exits, so those lines land after unittest's own "OK"
on stderr, and two of them in one shard pushed the "OK" out of the last few
lines: a passing shard read as failed (v0.5.7's darwin verify, 12 shards).

The suite runs here on a fake tests folder, one shard, the binaries faked
(BOWERY_BIN set, so nothing is built).
"""
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent

NOISY = '''
import unittest

class Noisy(unittest.TestCase):
    def test_{n}(self):
        print("\\n  a timing line from test {n}", end=" ")
'''

FAILING = '''
import unittest

class Failing(unittest.TestCase):
    def test_fails(self):
        self.fail("on purpose")
'''


class Verdict(unittest.TestCase):
    def suite(self, modules):
        d = pathlib.Path(tempfile.mkdtemp(prefix="test-suite-verdict."))
        self.addCleanup(shutil.rmtree, d, True)
        (d / "scripts").mkdir()
        shutil.copy(ROOT / "scripts" / "test-suite", d / "scripts" / "test-suite")
        (d / "tests").mkdir()
        for name, text in modules.items():
            (d / "tests" / f"{name}.py").write_text(text)
        env = {"PATH": "/usr/local/bin:/usr/bin:/bin", "BOWERY_BIN": str(d / "bowery"), "TMPDIR": str(d)}
        return subprocess.run(["bash", str(d / "scripts" / "test-suite"), "-j", "1"], env=env,
                              capture_output=True, text=True, timeout=120)

    def test_a_shard_that_printed_after_its_ok_passed(self):
        res = self.suite({f"test_noisy_{n}": NOISY.format(n=n) for n in range(4)})
        self.assertEqual(res.returncode, 0, res.stdout + res.stderr)
        self.assertIn(": OK", res.stdout)

    def test_a_failing_shard_still_fails(self):
        mods = {f"test_noisy_{n}": NOISY.format(n=n) for n in range(4)}
        mods["test_failing"] = FAILING
        res = self.suite(mods)
        self.assertEqual(res.returncode, 1, res.stdout + res.stderr)
        self.assertIn("shard-0 FAILED", res.stdout)
        self.assertIn("FAIL: test_fails", res.stdout)


if __name__ == "__main__":
    unittest.main()
