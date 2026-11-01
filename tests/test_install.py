"""install.sh: the Claude Code half against a throwaway config directory, and
a checkout install into a throwaway HOME, so nothing touches the real ~/.local.

    python3 -m unittest discover tests
"""

import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
INSTALL = str(ROOT / "install.sh")
ORIGINAL = "# My rules\n\nSome existing rule.\n"


class ClaudeSetup(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        t = Path(self.tmp.name)
        (t / "claude").mkdir()
        (t / "dots").mkdir()
        # CLAUDE.md as a dotfiles-style symlink: the case that a rename would break
        self.real = t / "dots" / "CLAUDE.md"
        self.real.write_text(ORIGINAL)
        self.link = t / "claude" / "CLAUDE.md"
        self.link.symlink_to("../dots/CLAUDE.md")
        self.skill = t / "claude" / "skills" / "bowery"
        self.env = {**os.environ, "CLAUDE_CONFIG_DIR": str(t / "claude")}

    def tearDown(self):
        self.tmp.cleanup()

    def run_(self, *args):
        p = subprocess.run([INSTALL, *args], env=self.env, capture_output=True, text=True,
                           stdin=subprocess.DEVNULL)
        self.assertEqual(p.returncode, 0, p.stderr)
        return p.stdout

    def test_without_a_terminal_it_asks_rather_than_writes(self):
        out = self.run_("--claude-only")
        self.assertIn("without asking", out)
        self.assertEqual(self.real.read_text(), ORIGINAL)
        self.assertTrue((self.skill / "SKILL.md").is_file(), "the skill is linked regardless")

    def test_section_is_added_once_and_through_the_symlink(self):
        self.run_("--claude-only", "--claude")
        self.run_("--claude-only", "--claude")
        text = self.real.read_text()
        self.assertTrue(text.startswith(ORIGINAL))
        self.assertEqual(text.count("bowery:begin"), 1)
        self.assertIn((ROOT / "agents" / "claude-md-snippet.md").read_text(), text)
        self.assertTrue(self.link.is_symlink(), "CLAUDE.md must stay a symlink")

    def test_no_claude_does_nothing(self):
        self.run_("--claude-only", "--no-claude")
        self.assertFalse(self.skill.exists())
        self.assertEqual(self.real.read_text(), ORIGINAL)

    def test_uninstall_restores_the_file_exactly(self):
        self.run_("--claude-only", "--claude")
        self.run_("--uninstall", "--claude-only")
        self.assertEqual(self.real.read_text(), ORIGINAL)
        self.assertFalse(self.skill.exists())
        self.assertTrue(self.link.is_symlink())

    def test_missing_claude_code_is_skipped(self):
        self.env["CLAUDE_CONFIG_DIR"] = str(Path(self.tmp.name) / "nope")
        self.assertIn("not found", self.run_("--claude-only", "--claude"))


class CheckoutInstall(unittest.TestCase):
    """./install.sh from this checkout, into a throwaway HOME."""

    def test_links_the_binary_and_completion_and_uninstalls(self):
        with tempfile.TemporaryDirectory() as h:
            env = {**os.environ, "HOME": h, "CLAUDE_CONFIG_DIR": str(Path(h) / "no-claude")}
            env.pop("XDG_DATA_HOME", None)
            p = subprocess.run([INSTALL, "--no-claude"], env=env, capture_output=True, text=True, stdin=subprocess.DEVNULL)
            self.assertEqual(p.returncode, 0, p.stderr)
            b = Path(h) / ".local" / "bin" / "bowery"
            self.assertTrue(b.is_symlink())
            self.assertEqual(os.path.realpath(b), str(ROOT / "build" / "bowery"))
            v = subprocess.run([str(b), "version"], capture_output=True, text=True)
            self.assertIn("bowery ", v.stdout)
            self.assertTrue((Path(h) / ".local/share/bash-completion/completions/bowery").is_symlink())
            p = subprocess.run([INSTALL, "--uninstall"], env=env, capture_output=True, text=True, stdin=subprocess.DEVNULL)
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertFalse(b.exists() or b.is_symlink())


if __name__ == "__main__":
    unittest.main()
