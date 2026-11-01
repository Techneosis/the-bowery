"""The config file and `bowery config`, version and help.

    python3 -m unittest discover tests
"""

import shutil
import unittest

from _fake import Bed
from _binary import ROOT


class Config(Bed):
    def test_defaults_without_a_file(self):
        out = self.ok("config")
        self.assertIn("defaults (no file)", out)
        self.assertIn('swarm = "swarm"', out)
        self.assertIn('sound_source = "bowery.sound"', out)
        self.assertIn('trust_roots = ["~/dev"]', out)
        self.assertIn(f"plus the herdr worktrees dir: {self.wt}", out)

    def test_the_example_file_is_valid(self):
        shutil.copy(ROOT / "examples" / "config.toml", self.config)
        out = self.ok("config")
        self.assertIn("[machines.swarm-box]", out)
        self.assertIn('identity_command = "~/bin/as"', out)

    def test_a_machine_without_identities_says_so(self):
        self.config.write_text('[machines.box]\nssh = "box"\n')
        self.assertIn("--as is refused here", self.ok("config"))

    def test_an_unknown_key_is_an_error_not_ignored(self):
        self.config.write_text('swram = "typo"\n')
        self.refused("unknown key(s) swram", "config")

    def test_a_broken_file_is_an_error(self):
        self.config.write_text('swarm = \n')
        self.refused(str(self.config), "list")

    def test_version_and_help(self):
        self.assertRegex(self.ok("version"), r"^bowery \S+ \(\w+/\w+\)")
        self.assertIn("bowery dispatch <name>", self.ok("--help"))
        self.refused("unknown command", "frobnicate-x")


if __name__ == "__main__":
    unittest.main()
