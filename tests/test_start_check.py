"""dispatch's start check: after the prompt, the agent has to start on it.

herdr's `agent prompt` can return with the text typed into the agent's
composer and never submitted; the agent then sits idle, and nothing says so.
The fake models that with FAKE_PROMPT_UNSUBMITTED=<n> (the prompt is
submitted by the n-th bare Enter; "never": by none). BOWERY_START_CHECK_MS
shortens the check's 60s (and its 15s after the extra Enter) for the tests.

    python3 -m unittest discover tests
"""

import json
import unittest

from _fake import BOX, Bed

FAST = {"BOWERY_START_CHECK_MS": "400"}


class StartCheck(Bed):
    def test_a_prompt_the_agent_takes_needs_no_help(self):
        self.dispatch("taken", "--cwd", self.repo, "--", "go", env=FAST)
        a = self.agent("taken")
        self.assertEqual(a["prompts"], [{"text": "go", "wait": False}])
        self.assertEqual(a["agent_status"], "done")
        self.assertNotIn("enters", a, "no Enter for an agent that started")

    def test_an_unsubmitted_prompt_gets_one_more_enter(self):
        out = self.dispatch("nudged", "--cwd", self.repo, "--", "go", env={**FAST, "FAKE_PROMPT_UNSUBMITTED": "1"})
        self.assertEqual(out["name"], "nudged")
        a = self.agent("nudged")
        self.assertEqual(a.get("enters"), 1)
        self.assertEqual(a["agent_status"], "done", "the Enter submitted it")

    def test_an_agent_that_never_starts_fails_loudly_and_stays_up(self):
        rc, out = self.bowery("dispatch", "stuck", "--cwd", self.repo, "--", "go",
                              env={**FAST, "FAKE_PROMPT_UNSUBMITTED": "never"})
        self.assertEqual(rc, 1, out)
        self.assertIn("agent stuck never started working; its prompt may be unsubmitted: herdr agent read stuck", out)
        a = self.agent("stuck")
        self.assertIsNotNone(a, "the agent is kept, not torn down")
        self.assertEqual(a.get("enters"), 1, "Enter is re-sent once, not more")
        self.assertEqual(a["agent_status"], "idle")
        self.assertEqual(len(self.records("stuck")), 1, "and its record stays, so done and resume can act on it")
        printed = json.loads(next(l for l in out.splitlines() if l.startswith("{")))
        self.assertEqual(printed["name"], "stuck", "the dispatch JSON is still printed, for the caller to find it")
        self.ok("done", "stuck")

    def test_no_start_check_skips_it(self):
        self.dispatch("idler", "--no-start-check", "--cwd", self.repo, "--", "go",
                      env={**FAST, "FAKE_PROMPT_UNSUBMITTED": "never"})
        a = self.agent("idler")
        self.assertNotIn("enters", a)
        self.assertEqual(a["agent_status"], "idle")

    def test_without_a_prompt_there_is_nothing_to_check(self):
        self.dispatch("bare", "--cwd", self.repo, env={**FAST, "FAKE_PROMPT_UNSUBMITTED": "never"})
        self.assertEqual(self.agent("bare")["agent_status"], "idle")
        self.assertNotIn("enters", self.agent("bare"))

    def test_wait_has_already_seen_it_settle(self):
        self.dispatch("waited", "--wait", "--cwd", self.repo, "--", "go",
                      env={**FAST, "FAKE_PROMPT_UNSUBMITTED": "never"})
        self.assertNotIn("enters", self.agent("waited"))

    def test_on_a_machine_the_hint_names_it(self):
        self.add_box()
        self.box_repo()
        rc, out = self.bowery("dispatch", "far", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app", "--", "go",
                              env={**FAST, "FAKE_PROMPT_UNSUBMITTED": "never"})
        self.assertEqual(rc, 1, out)
        self.assertIn(f"agent far never started working; its prompt may be unsubmitted: herdr --machine {BOX} agent read far",
                      out)
        a = self.agent("far", BOX)
        self.assertEqual(a.get("enters"), 1, "the Enter went to the box's server")
        self.assertIsNone(self.agent("far"), "nothing on the local server")


if __name__ == "__main__":
    unittest.main()
