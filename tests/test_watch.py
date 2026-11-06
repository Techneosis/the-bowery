"""bowery watch: wakes on transitions, stays quiet on progress, heartbeats."""

import json
import os
import stat
import unittest

from _fake import Bed


class Watch(Bed):
    def setUp(self):
        super().setUp()
        self.cfg = self.t / "watch.cfg"
        bin_ = self.t / "bin"
        bin_.mkdir()
        gh = bin_ / "gh"  # `gh pr view N ...`: the JSON in gh-pr-N
        gh.write_text(f'#!/bin/sh\ncat "{self.t}/gh-pr-$3" 2>/dev/null || exit 1\n')
        gh.chmod(0o755)
        self.env["PATH"] = f"{bin_}:{self.env['PATH']}"
        self.plugins = self.home / ".config" / "bowery" / "watch.d"
        self.plugins.mkdir(parents=True)

    def watch(self, *lines):
        self.cfg.write_text("\n".join(lines) + "\n")
        return self.ok("watch", self.cfg, "--poll", "1s", "--max", "2s")

    def plugin(self, kind, value):
        p = self.plugins / kind
        p.write_text(f"#!/bin/sh\necho 'some notice first'\necho '{value}'\n")
        p.chmod(p.stat().st_mode | stat.S_IEXEC)

    def test_first_run_records_and_heartbeats(self):
        self.dispatch("w1", "--cwd", self.repo)
        out = self.watch("agent w1", "agent nobody")
        self.assertIn("HEARTBEAT", out)
        self.assertIn("agent w1 idle", out)
        self.assertIn("agent nobody gone", out)
        self.assertIn("agent w1\tidle", (self.t / "watch.cfg.state").read_text())

    def test_an_agent_finishing_wakes(self):
        self.dispatch("w1", "--cwd", self.repo)
        self.watch("agent w1")
        self.set_status("w1", "done")
        out = self.watch("agent w1")
        self.assertTrue(out.startswith("CHANGED:"), out)
        self.assertIn("agent w1 done", out)

    def test_working_is_quiet(self):
        self.dispatch("w1", "--cwd", self.repo)
        self.watch("agent w1")
        self.set_status("w1", "working")
        self.assertIn("HEARTBEAT", self.watch("agent w1"))

    def test_a_blocked_agent_wakes_on_the_first_poll(self):
        self.dispatch("w1", "--cwd", self.repo)
        self.watch("agent w1")
        self.set_status("w1", "blocked")
        self.assertIn("CHANGED:", self.watch("agent w1"))
        out = self.watch("agent w1")  # still blocked next time: say so at once
        self.assertTrue(out.startswith("STILL BLOCKED:"), out)

    def test_a_pr_merging_wakes(self):
        (self.t / "gh-pr-7").write_text(json.dumps({"state": "OPEN", "mergeable": "UNKNOWN"}))
        self.watch("repo acme/web", "pr 7")
        (self.t / "gh-pr-7").write_text(json.dumps({"state": "OPEN", "mergeable": "MERGEABLE"}))
        self.assertIn("HEARTBEAT", self.watch("repo acme/web", "pr 7"), "mergeable recomputing is noise")
        (self.t / "gh-pr-7").write_text(json.dumps({"state": "MERGED", "mergeable": "UNKNOWN"}))
        out = self.watch("repo acme/web", "pr 7")
        self.assertIn("pr 7 MERGED", out.split("ALL:")[0])

    def test_a_failed_read_is_not_a_change(self):
        (self.t / "gh-pr-7").write_text(json.dumps({"state": "OPEN"}))
        self.watch("repo acme/web", "pr 7")
        (self.t / "gh-pr-7").unlink()
        self.assertIn("HEARTBEAT", self.watch("repo acme/web", "pr 7"))
        (self.t / "gh-pr-7").write_text(json.dumps({"state": "OPEN"}))
        self.assertIn("HEARTBEAT", self.watch("repo acme/web", "pr 7"), "back to the same value: nothing happened")

    def test_plugin_kinds(self):
        self.plugin("ticket", "open")
        self.watch("ticket T-1")
        self.plugin("ticket", "~open: 3 of 5 checks")
        self.assertIn("HEARTBEAT", self.watch("ticket T-1"), "a ~value is quiet")
        self.plugin("ticket", "closed")
        out = self.watch("ticket T-1")
        self.assertIn("ticket T-1 closed", out.split("ALL:")[0])
        self.assertIn("unknown-kind", self.watch("nosuch x"))


if __name__ == "__main__":
    unittest.main()
