"""resume: a dead agent started again with its conversation, as it was.

    python3 -m unittest discover tests
"""

import json
import os
import shutil
import subprocess

from _fake import BOX, Bed, sh

NOTICE = "You were resumed: check what you already did before redoing anything."


class Resume(Bed):
    def kill(self, name, machine=None):
        """The agent's tab goes (herdr restarted, the pane died, closed by
        mistake): no agent, no pane."""
        tab = self.record(name)["tab_id"]
        self.herdr("tab", "close", tab, machine=machine)
        self.assertIsNone(self.agent(name, machine))

    def exit_to_shell(self, name, machine=None):
        """The agent quit; its pane is left at a shell prompt."""
        with self.mutate(machine) as s:
            del s["agents"][name]

    def start_by_hand(self, name, pane, *args):
        """An agent someone else started, in a pane whose shell is up."""
        with self.mutate() as s:
            next(p for p in s["panes"] if p["pane_id"] == pane)["warmup"] = 0
            s["trusted"] = s.get("trusted", []) + [str(self.t)]
        self.herdr("agent", "start", name, "--kind", "claude", "--pane", pane, *args)

    def resume(self, name, *args, **kw):
        out = self.ok("resume", name, *args, **kw)
        return json.loads(out.strip().splitlines()[-1])

    def unchanged(self, name, machine=None):
        """A snapshot to compare after a refusal: nothing may have moved."""
        s = self.state_of(machine)
        return (sorted(s["agents"]), len(s["panes"]), len(s["workspaces"]), self.records(name)[0].read_text())

    # ------------------------------------------------------------ the record

    def test_dispatch_records_the_session_directory_and_flags(self):
        self.dispatch("rec", "--worktree", "rec", "--cwd", self.repo, "--gated", "--model", "opus")
        r, a = self.record("rec"), self.agent("rec")
        self.assertEqual(r["session"], a["session"])
        self.assertTrue(r["session"])
        self.assertEqual(r["cwd"], str(self.wt / "repo" / "rec"))
        self.assertEqual((r["gated"], r["model"]), (True, "opus"))
        self.assertNotIn("resumes", r, "not resumed yet")

    def test_the_session_is_kept_fresh_while_the_agent_lives(self):
        self.dispatch("fresh", "--cwd", self.repo)
        with self.mutate() as s:  # a /clear: claude starts a new session
            s["agents"]["fresh"]["session"] = "after-clear"
        self.ok("list")
        self.assertEqual(self.record("fresh")["session"], "after-clear")
        with self.mutate() as s:
            s["agents"]["fresh"]["session"] = "again"
        self.ok("orphans")
        self.ok("orphans", "--json")
        self.assertEqual(self.record("fresh")["session"], "after-clear", "orphans is read-only")
        self.ok("list")
        self.assertEqual(self.record("fresh")["session"], "again")

    def test_a_refused_or_dry_run_resume_writes_no_record_at_all(self):
        self.dispatch("other", "--cwd", self.repo)
        self.dispatch("dry2", "--cwd", self.repo)
        self.kill("dry2")
        with self.mutate() as s:
            s["agents"]["other"]["session"] = "after-clear"
        files = {f.name: f.read_text() for f in self.records()}
        self.ok("resume", "dry2", "--dry-run")
        self.refused("still alive", "resume", "other")
        self.assertEqual({f.name: f.read_text() for f in self.records()}, files, "not even another agent's record")

    def test_an_old_record_without_a_terminal_id_takes_no_namesakes_session(self):
        self.dispatch("legacy", "--cwd", self.repo)
        f = self.records("legacy")[0]
        rec = json.loads(f.read_text())
        rec.pop("terminal_id")
        f.write_text(json.dumps(rec))
        self.kill("legacy")
        p = self.new_workspace("elsewhere", str(self.repo))
        self.start_by_hand("legacy", p["pane_id"])
        self.ok("list")
        self.assertEqual(json.loads(f.read_text())["session"], rec["session"])

    def test_a_namesake_in_another_terminal_does_not_overwrite_the_session(self):
        self.dispatch("same", "--cwd", self.repo)
        sess = self.record("same")["session"]
        self.kill("same")
        p = self.new_workspace("elsewhere", str(self.repo))
        self.start_by_hand("same", p["pane_id"])
        self.ok("list")
        self.assertEqual(self.record("same")["session"], sess)

    # ------------------------------------------------------------ happy paths

    def test_a_dead_agent_comes_back_in_its_worktree_with_its_session(self):
        self.dispatch("back", "--worktree", "back", "--cwd", self.repo, "--", "do the thing")
        before = self.record("back")
        self.kill("back")
        self.assertIsNone(self.swarm(), "swarm went with its last tab")
        out = self.resume("back")
        a = self.agent("back")
        self.assertEqual(a["agent_status"], "idle")
        self.assertEqual(a["args"], ["--dangerously-skip-permissions", "--resume", before["session"]])
        self.assertEqual(self.pane(a["pane_id"])["cwd"], before["cwd"], "started where the session lives")
        self.assertTrue(a["prompts"][0]["text"].startswith(NOTICE), a["prompts"])
        head = sh("git", "-C", before["worktree"], "rev-parse", "--short", "HEAD")
        self.assertIn(f"is on back at {head}; compare with git log.", a["prompts"][0]["text"])
        self.assertEqual(self.pane(a["pane_id"])["tokens"], {"sound": "off"}, "still quiet")
        self.assertEqual(self.tab_labels(self.swarm()["workspace_id"]), ["back"])
        r = self.record("back")
        self.assertEqual((r["resumes"], r["session"], r["pane_id"]), (1, before["session"], out["pane_id"]))
        self.assertTrue(r["resumed"])
        self.assertEqual(r["owns"], {"kind": "tab", "id": out["tab_id"]})
        self.assertEqual((out["session"], out["resumes"]), (before["session"], "1"))
        self.assertEqual(r["worktree"], before["worktree"])

        self.kill("back")
        self.resume("back")
        self.assertEqual(self.record("back")["resumes"], 2, "counted each time")

        # done brings it home as usual.
        self.ok("done", "back", "--force")
        self.assertIsNone(self.agent("back"))
        self.assertEqual(self.records(), [])

    def test_an_agent_that_exited_comes_back_in_its_own_pane(self):
        a = self.dispatch("quit", "--cwd", self.repo)
        self.exit_to_shell("quit")
        out = self.resume("quit")
        self.assertEqual(out["pane_id"], a["pane_id"], "its own terminal, reused")
        self.assertEqual(len(self.swarm()["tabs"]), 1, "no new tab")
        self.assertEqual(self.agent("quit")["pane_id"], a["pane_id"])

    def test_a_reused_pane_is_moved_back_to_the_agents_directory(self):
        a = self.dispatch("moved", "--cwd", self.repo)
        self.exit_to_shell("moved")
        with self.mutate() as s:
            next(p for p in s["panes"] if p["pane_id"] == a["pane_id"])["cwd"] = str(self.t)
        self.ok("resume", "moved")
        log = self.state_of()["log"]
        self.assertIn(["pane", "run", a["pane_id"], f"cd {self.repo}"], log)

    def test_its_own_tab_still_open_gets_a_new_pane(self):
        self.dispatch("keep", "--cwd", self.repo)
        a = self.dispatch("inside", "--cwd", self.repo)
        shell = self.split_off(a["pane_id"])
        self.herdr("pane", "close", a["pane_id"])
        out = self.resume("inside")
        self.assertEqual(out["tab_id"], a["tab_id"], "back in the tab bowery made for it")
        self.assertEqual(self.tab_labels(self.swarm()["workspace_id"]), ["keep", "inside"], "no second tab")
        self.assertIsNotNone(self.pane(shell["pane_id"]))
        self.assertEqual(self.record("inside")["owns"], {"kind": "tab", "id": a["tab_id"]})

    def test_herdr_failing_to_answer_is_not_a_closed_tab(self):
        # A hiccup on `tab get` used to read as "its tab is gone": the resume
        # then opened a second tab beside the old one, and orphans lost it.
        self.dispatch("keep", "--cwd", self.repo)
        a = self.dispatch("hic", "--cwd", self.repo)
        self.split_off(a["pane_id"])
        self.herdr("pane", "close", a["pane_id"])
        snap = self.unchanged("hic")
        env = {"FAKE_TAB_GET_FAIL": "1"}
        self.refused("could not answer", "resume", "hic", env=env)
        self.refused("could not answer", "resume", "hic", "--swarm", env=env)
        self.assertEqual(self.unchanged("hic"), snap)
        self.assertEqual(self.tab_labels(self.swarm()["workspace_id"]), ["keep", "hic"], "no second tab")
        self.refused("could not answer", "orphans", env=env)
        self.refused("could not answer", "done", "hic", env=env)
        self.assertIsNotNone(self.record("hic"))

    def test_a_prompt_follows_the_notice_and_the_flags_carry_over(self):
        self.dispatch("flags", "--cwd", self.repo, "--gated", "--model", "opus")
        self.kill("flags")
        self.resume("flags", "--", "and", "then", "finish")
        a = self.agent("flags")
        self.assertEqual(a["args"], ["--model", "opus", "--resume", self.record("flags")["session"]])
        self.assertEqual(len(a["prompts"]), 1)
        self.assertTrue(a["prompts"][0]["text"].startswith(NOTICE))
        self.assertTrue(a["prompts"][0]["text"].endswith("\n\nand then finish"))

    def test_a_workspace_agent_gets_a_workspace_and_swarm_can_be_asked_for(self):
        self.dispatch("own", "--workspace", "--cwd", self.repo)
        self.herdr("workspace", "close", self.record("own")["workspace_id"])
        out = self.resume("own")
        self.assertIn("own", self.ws_labels())
        self.assertEqual(self.record("own")["owns"], {"kind": "workspace", "id": out["workspace_id"]})
        self.herdr("workspace", "close", out["workspace_id"])
        out = self.resume("own", "--swarm")
        self.assertEqual(self.tab_labels(self.swarm()["workspace_id"]), ["own"])
        self.assertEqual(self.record("own")["owns"], {"kind": "tab", "id": out["tab_id"]})

    def test_a_split_agent_comes_back_on_the_rooftop_not_beside_its_neighbour(self):
        host = self.new_workspace("host", str(self.repo))
        self.dispatch("beside", "--split", "--from", host["pane_id"], "--cwd", self.repo)
        self.herdr("pane", "close", self.record("beside")["pane_id"])
        out = self.resume("beside")
        self.assertEqual(self.tab_labels(self.swarm()["workspace_id"]), ["beside"])
        self.assertNotEqual(out["workspace_id"], host["workspace_id"])

    def test_dry_run_says_where_and_changes_nothing(self):
        self.dispatch("dry", "--cwd", self.repo)
        self.kill("dry")
        before = self.state_of()["workspaces"], self.records("dry")[0].read_text()
        out = self.ok("resume", "dry", "--dry-run")
        self.assertIn("would resume dry", out)
        self.assertIn("a new tab dry in swarm", out)
        self.assertEqual((self.state_of()["workspaces"], self.records("dry")[0].read_text()), before)

    # ------------------------------------------------------------ refusals

    def test_refused_while_alive(self):
        self.dispatch("up", "--cwd", self.repo)
        snap = self.unchanged("up")
        self.refused("still alive", "resume", "up")
        self.assertEqual(self.unchanged("up"), snap)

    def test_refused_without_a_record(self):
        p = self.new_workspace("hand", str(self.repo))
        self.start_by_hand("loose", p["pane_id"])
        self.refused("no dispatch record for loose", "resume", "loose")
        self.refused("no dispatch record for nobody", "resume", "nobody")

    def test_refused_without_a_session_and_an_old_record_still_loads(self):
        a = self.dispatch("old", "--cwd", self.repo)
        f = self.records("old")[0]
        rec = json.loads(f.read_text())
        for k in ("session", "cwd", "gated", "model", "terminal_id", "owns"):
            rec.pop(k, None)
        rec["branch"] = None  # as records from before owns and branches were written
        f.write_text(json.dumps(rec))
        self.herdr("tab", "close", self.agent("old")["tab_id"] if "tab_id" in self.agent("old") else a["tab_id"])
        self.refused("has no session id", "resume", "old")
        self.ok("list")
        self.ok("orphans")
        self.assertNotIn("session", json.loads(f.read_text()), "nothing invented")
        out = json.loads(self.ok("orphans", "--json"))
        self.assertEqual([o["resumable"] for o in out if o["name"] == "old"], [False], "no folder to start it in")

    def test_refused_when_its_worktree_is_gone(self):
        self.dispatch("wt", "--worktree", "wt", "--cwd", self.repo)
        self.kill("wt")
        sh("git", "-C", self.repo, "worktree", "remove", "--force", str(self.wt / "repo" / "wt"))
        snap = self.unchanged("wt")
        self.refused("is gone", "resume", "wt")
        self.assertEqual(self.unchanged("wt"), snap)

    def test_refused_when_an_also_worktree_is_gone(self):
        extra = self.wt / "repo" / "extra"
        sh("git", "-C", self.repo, "worktree", "add", "-q", "-b", "extra", str(extra), "origin/main")
        self.dispatch("two", "--cwd", self.repo, "--also-worktree", extra)
        self.kill("two")
        shutil.rmtree(extra)
        self.refused(f"worktree {extra} is gone", "resume", "two")

    def test_refused_when_its_directory_is_gone(self):
        d = self.t / "scratch"
        d.mkdir()
        self.dispatch("nodir", "--cwd", d)
        self.kill("nodir")
        d.rmdir()
        self.refused("which is gone", "resume", "nodir")

    def test_refused_when_its_pane_now_holds_someone_else(self):
        a = self.dispatch("taken", "--cwd", self.repo)
        self.exit_to_shell("taken")
        self.herdr("agent", "start", "squatter", "--kind", "claude", "--pane", a["pane_id"])
        snap = self.unchanged("taken")
        self.refused("now holds squatter", "resume", "taken")
        self.assertEqual(self.unchanged("taken"), snap)

    def test_refused_when_its_pane_is_running_something(self):
        a = self.dispatch("busy", "--cwd", self.repo)
        self.exit_to_shell("busy")
        self.run_in(a["pane_id"], "sleep 300")
        self.refused("not sitting at a shell prompt", "resume", "busy")
        self.assertIsNone(self.agent("busy"))

    def test_refused_when_the_session_is_live_elsewhere(self):
        self.dispatch("twice", "--cwd", self.repo)
        sess = self.record("twice")["session"]
        self.kill("twice")
        p = self.new_workspace("other", str(self.repo))
        self.start_by_hand("copy", p["pane_id"], "--", "--resume", sess)
        self.refused("can't run twice", "resume", "twice")

    def test_refused_for_a_kind_it_cannot_resume(self):
        self.dispatch("cx", "--kind", "codex", "--cwd", self.repo)
        self.kill("cx")
        self.refused("claude sessions only", "resume", "cx")

    def test_never_starts_in_a_pane_carrying_another_tools_marker(self):
        self.config.write_text('hands_off_env = ["OTHER_TOOL_TAG"]\n')
        a = self.dispatch("marked", "--cwd", self.repo)
        self.exit_to_shell("marked")
        proc = subprocess.Popen(["sleep", "300"], env={**os.environ, "OTHER_TOOL_TAG": "x-1"})
        self.addCleanup(proc.wait)
        self.addCleanup(proc.kill)
        with self.mutate() as s:
            next(p for p in s["panes"] if p["pane_id"] == a["pane_id"])["pid"] = proc.pid
        snap = self.unchanged("marked")
        self.refused("carrying OTHER_TOOL_TAG", "resume", "marked")
        self.assertEqual(self.unchanged("marked"), snap)
        # Without the marker the same pane is fine.
        self.config.write_text('hands_off_env = ["SOMETHING_ELSE"]\n')
        self.ok("resume", "marked")

    def test_a_first_prompt_that_never_arrives_takes_the_start_back(self):
        self.dispatch("late", "--cwd", self.repo)
        self.kill("late")
        before = self.records("late")[0].read_text()
        rc, out = self.bowery("resume", "late", env={"FAKE_PROMPT_FAIL": "1"})
        self.assertNotEqual(rc, 0)
        self.assertIn("first prompt didn't reach it", out)
        self.assertIsNone(self.agent("late"), "a seat with no reason to be up isn't left idle")
        self.assertIsNone(self.swarm(), "the tab it made is taken back")
        self.assertEqual(self.records("late")[0].read_text(), before, "nothing recorded, so a retry isn't 'too soon'")
        self.resume("late")

    def test_a_new_place_is_refused_while_the_old_one_is_open(self):
        a = self.dispatch("stay", "--cwd", self.repo)
        self.exit_to_shell("stay")
        snap = self.unchanged("stay")
        self.refused("still open", "resume", "stay", "--swarm")
        self.assertEqual(self.unchanged("stay"), snap)
        self.assertEqual(self.resume("stay")["pane_id"], a["pane_id"])

    def test_bad_options_are_refused_before_anything(self):
        self.dispatch("opts", "--cwd", self.repo)
        self.kill("opts")
        snap = self.unchanged("opts")
        self.refused("milliseconds", "resume", "opts", "--timeout", "10s")
        self.refused("pick one", "resume", "opts", "--swarm", "--workspace")
        self.assertEqual(self.unchanged("opts"), snap)

    def test_the_prompt_is_taken_whole_and_options_may_come_first(self):
        self.dispatch("words", "--cwd", self.repo)
        self.kill("words")
        self.ok("resume", "--swarm", "words", "--", "check", "--machine", "x", "--dry-run")
        a = self.agent("words")
        self.assertTrue(a["prompts"][0]["text"].endswith("\n\ncheck --machine x --dry-run"))

    def test_an_older_record_must_say_whether_it_was_gated(self):
        self.dispatch("pre", "--cwd", self.repo, "--gated")
        f = self.records("pre")[0]
        rec = json.loads(f.read_text())
        for k in ("cwd", "gated", "model"):
            rec.pop(k, None)
        rec["worktree"] = str(self.repo)  # an older record: its directory was its worktree
        f.write_text(json.dumps(rec))
        self.kill("pre")
        self.refused("whether it was --gated", "resume", "pre")
        self.resume("pre", "--gated")
        self.assertEqual(self.agent("pre")["args"], ["--resume", rec["session"]], "prompts on, as asked")
        self.assertTrue(self.record("pre")["gated"])

    def test_a_split_beside_a_marked_neighbour_is_not_refused(self):
        self.config.write_text('hands_off_env = ["OTHER_TOOL_TAG"]\n')
        self.dispatch("keep2", "--cwd", self.repo)
        a = self.dispatch("nb", "--cwd", self.repo)
        shell = self.split_off(a["pane_id"])
        proc = subprocess.Popen(["sleep", "300"], env={**os.environ, "OTHER_TOOL_TAG": "x-1"})
        self.addCleanup(proc.wait)
        self.addCleanup(proc.kill)
        with self.mutate() as s:
            next(p for p in s["panes"] if p["pane_id"] == shell["pane_id"])["pid"] = proc.pid
        self.herdr("pane", "close", a["pane_id"])
        out = self.resume("nb")
        self.assertEqual(out["tab_id"], a["tab_id"], "a fresh pane of its own, in its own tab")

    def test_an_environment_it_cannot_read_is_refused(self):
        self.config.write_text('hands_off_env = ["OTHER_TOOL_TAG"]\n')
        a = self.dispatch("unread", "--cwd", self.repo)
        self.exit_to_shell("unread")
        with self.mutate() as s:  # a pid that isn't there (or isn't ours to read)
            next(p for p in s["panes"] if p["pane_id"] == a["pane_id"])["pid"] = 4194301
        snap = self.unchanged("unread")
        self.refused("can't read the environment of pid 4194301", "resume", "unread")
        self.assertEqual(self.unchanged("unread"), snap)
        if os.getuid() != 0 and not os.access("/proc/1/environ", os.R_OK):
            # A live process of another user (pid 1, root's): /proc won't say.
            with self.mutate() as s:
                next(p for p in s["panes"] if p["pane_id"] == a["pane_id"])["pid"] = 1
            self.refused("can't read the environment of pid 1", "resume", "unread")
        # Where ps -E stands in for /proc (a Mac): the environment is what -E
        # adds after the plain command, and nothing else counts. The pid has
        # no /proc entry, so the shim is all there is to read.
        bin_ = self.t / "macps"
        bin_.mkdir()
        shim = bin_ / "ps"
        mac = {"PATH": f"{bin_}:{self.env['PATH']}"}
        with self.mutate() as s:
            next(p for p in s["panes"] if p["pane_id"] == a["pane_id"])["pid"] = 4194301

        def ps(command, environ):
            shim.write_text('#!/bin/sh\ncase "$*" in *E*) echo "%s%s";; *) echo "%s";; esac\n'
                            % (command, environ, command))
            shim.chmod(0o755)

        ps("/sbin/launchd", "")  # another user's: the command alone, exit 0
        self.refused("can't read the environment of pid 4194301", "resume", "unread", env=mac)
        ps("env HOME=/x OTHER_TOOL_TAG=1 tool", "")  # markers in the arguments are not an environment
        self.refused("can't read the environment of pid 4194301", "resume", "unread", env=mac)
        ps("tool --flag", " HOME=/home/x PATH=/usr/bin OTHER_TOOL_TAG=1")
        self.refused("carrying OTHER_TOOL_TAG", "resume", "unread", env=mac)
        self.assertEqual(self.unchanged("unread"), snap)
        ps("tool OTHER_TOOL_TAG=1", " HOME=/home/x PATH=/usr/bin")  # in argv only: readable, unmarked
        self.ok("resume", "unread", env=mac)

    def test_a_failed_start_in_its_own_pane_types_nothing_into_what_it_left(self):
        a = self.dispatch("stuck", "--cwd", self.repo)
        self.exit_to_shell("stuck")
        with self.mutate() as s:
            s["trusted"] = []  # the folder is asked about again, and the answer won't take
            next(p for p in s["panes"] if p["pane_id"] == a["pane_id"])["cwd"] = str(self.t)
        rc, out = self.bowery("resume", "stuck", "--timeout", "500", env={"FAKE_TRUST_STUCK": "1"})
        self.assertNotEqual(rc, 0, out)
        log = self.state_of()["log"]
        self.assertNotIn(["pane", "run", a["pane_id"], f"cd {self.t}"], log, "no cd typed into a half-started claude")
        self.assertIsNone(self.pane(a["pane_id"]), "bowery's own pane, closed with what was left in it")

    def test_a_bad_hands_off_name_is_a_config_error(self):
        self.config.write_text('hands_off_env = [""]\n')
        self.refused("not an environment variable name", "config")

    # ------------------------------------------------------------ a launcher

    def launcher(self, name="seat-launch", body='export SEAT_BOX=box-"$1"; shift; exec claude "$@"'):
        f = self.t / name
        f.write_text("#!/bin/bash\n" + body + "\n")
        f.chmod(0o755)
        return f

    def test_a_launcher_starts_the_agent_and_resume_reuses_it(self):
        seat = self.launcher()
        out = self.dispatch("seated", "--cwd", self.repo, "--launcher", f"{seat} seated", "--", "hello")
        a = self.agent("seated")
        self.assertEqual(a["pane_id"], out["pane_id"], "named, in the pane bowery made")
        self.assertEqual(a["env"].get("SEAT_BOX"), "box-seated", "started through the launcher")
        self.assertEqual(a["env"].get("BOWERY_LAUNCH"), "1", "told the Bowery is starting it")
        self.assertEqual(a["args"], ["--dangerously-skip-permissions"])
        self.assertEqual([p["text"] for p in a["prompts"]], ["hello"])
        rec = self.record("seated")
        self.assertEqual(rec["launcher"], f"{seat} seated")
        self.assertEqual(rec["session"], a["session"])

        self.kill("seated")
        self.resume("seated")
        a = self.agent("seated")
        self.assertEqual(a["env"].get("SEAT_BOX"), "box-seated", "the record's launcher, again")
        self.assertEqual(a["args"], ["--dangerously-skip-permissions", "--resume", rec["session"]])
        self.assertTrue(a["prompts"][0]["text"].startswith(NOTICE))

    def test_resume_can_add_or_drop_a_launcher(self):
        seat = self.launcher()
        self.dispatch("later", "--cwd", self.repo)
        self.kill("later")
        self.assertIn(f"through {seat} later", self.ok("resume", "later", "--launcher", f"{seat} later", "--dry-run"))
        self.resume("later", "--launcher", f"{seat} later")
        self.assertEqual(self.agent("later")["env"].get("SEAT_BOX"), "box-later")
        self.assertEqual(self.record("later")["launcher"], f"{seat} later")
        self.kill("later")
        self.resume("later", "--no-launcher")
        self.assertNotIn("SEAT_BOX", self.agent("later")["env"])
        self.assertNotIn("launcher", self.record("later"))
        self.refused("contradict", "resume", "later", "--launcher", "x", "--no-launcher")

    def test_a_launcher_that_does_not_start_claude_leaves_nothing(self):
        bad = self.launcher("not-claude", "echo hello from the launcher")
        rc, out = self.bowery("dispatch", "nope", "--cwd", self.repo, "--launcher", str(bad), "--timeout", "1500")
        self.assertNotEqual(rc, 0, out)
        self.assertIn("must exec claude", out)
        self.assertIn("hello from the launcher", out)
        self.assertIsNone(self.agent("nope"))
        self.assertEqual(self.records("nope"), [])
        self.assertIsNone(self.swarm(), "the tab it made is taken back")

    def test_a_launcher_must_be_one_plain_command(self):
        self.dispatch("plain", "--cwd", self.repo)
        self.kill("plain")
        snap = self.unchanged("plain")
        self.refused("not the option --dry-run", "resume", "plain", "--launcher", "--dry-run")
        self.refused("control character", "resume", "plain", "--launcher", "seat\x03 plain")
        self.refused("control character", "resume", "plain", "--launcher", "seat\u202e plain")
        self.refused("it's empty", "resume", "plain", "--launcher=")
        self.assertEqual(self.unchanged("plain"), snap)
        self.refused("it's empty", "dispatch", "plain2", "--cwd", self.repo, "--launcher", "")
        self.assertIsNone(self.agent("plain2"))
        seat = self.launcher()
        self.resume("plain", f"--launcher={seat} plain")
        self.assertEqual(self.agent("plain")["env"].get("SEAT_BOX"), "box-plain", "the = form")

    def test_a_launcher_that_drops_the_session_is_refused(self):
        self.dispatch("drops", "--cwd", self.repo)
        sess = self.record("drops")["session"]
        self.kill("drops")
        lossy = self.launcher("lossy", "exec claude")  # throws claude's arguments away
        rc, out = self.bowery("resume", "drops", "--launcher", str(lossy))
        self.assertNotEqual(rc, 0, out)
        self.assertIn(f"not {sess}", out)
        self.assertEqual(self.record("drops")["session"], sess, "the record keeps its conversation")
        self.assertNotIn("drops", self.agent_names(), "the wrong claude doesn't keep the agent's name")
        self.assertIsNone(self.agent("drops"))
        self.assertIsNone(self.swarm(), "the tab it made is taken back")

    def test_a_failed_launcher_in_a_pane_that_isnt_bowerys_warns(self):
        p = self.new_workspace("mine", str(self.repo))
        bad = self.launcher("not-claude", "echo hello from the launcher")
        rc, out = self.bowery("dispatch", "inmine", "--pane", p["pane_id"], "--launcher", str(bad), "--timeout", "1500")
        self.assertNotEqual(rc, 0, out)
        self.assertIn(f"was run in your pane {p['pane_id']}", out)
        self.assertIsNotNone(self.pane(p["pane_id"]), "the person's pane is left open")
        # Failing before anything is typed says nothing of the kind.
        with self.mutate() as s:
            next(x for x in s["panes"] if x["pane_id"] == p["pane_id"])["busy"] = "vim"
        rc, out = self.bowery("dispatch", "inmine2", "--pane", p["pane_id"], "--launcher", str(bad), "--timeout", "1000")
        self.assertNotEqual(rc, 0, out)
        self.assertNotIn("was run in your pane", out)

    def test_a_failed_launcher_in_its_own_pane_is_undone_safely(self):
        a = self.dispatch("own2", "--cwd", self.repo)
        self.exit_to_shell("own2")
        bad = self.launcher("not-claude", "echo hello from the launcher")
        rc, out = self.bowery("resume", "own2", "--launcher", str(bad), "--timeout", "1500")
        self.assertNotEqual(rc, 0, out)
        self.assertIn("must exec claude", out)
        self.assertIsNotNone(self.pane(a["pane_id"]), "a bare shell is left as it was")
        self.assertNotIn("launcher", self.record("own2"), "nothing recorded")

    def test_a_stored_launcher_is_checked_again(self):
        self.dispatch("stored", "--cwd", self.repo)
        self.kill("stored")
        f = self.records("stored")[0]
        rec = json.loads(f.read_text())
        rec["launcher"] = "seat\x1b[2J stored"  # written by hand, or by an older bowery
        f.write_text(json.dumps(rec))
        self.refused("recorded launcher takes one plain command line", "resume", "stored")
        self.ok("resume", "stored", "--no-launcher")

    def test_a_launcher_needs_claude(self):
        self.refused("only supported with --kind claude", "dispatch", "cx2", "--kind", "codex", "--launcher", "x")

    # ------------------------------------------------------------ unattended: --if-dead, --not-within, the lock

    def test_if_dead_does_nothing_to_a_live_agent_and_resumes_a_dead_one(self):
        self.dispatch("gone1", "--cwd", self.repo)
        snap = self.unchanged("gone1")
        out = self.ok("resume", "gone1", "--if-dead")
        self.assertIn("alive", out)
        self.assertIn("nothing to do", out)
        self.assertEqual(self.unchanged("gone1"), snap)
        self.kill("gone1")
        self.ok("resume", "gone1", "--if-dead", "--", "1 unread letter")
        self.assertTrue(self.agent("gone1")["prompts"][0]["text"].endswith("\n\n1 unread letter"))
        # Other refusals still fail under --if-dead: a hook must see them.
        self.refused("no dispatch record", "resume", "nobody", "--if-dead")

    def test_not_within_keeps_a_crash_looping_agent_from_restarting(self):
        self.dispatch("loop", "--cwd", self.repo)
        self.kill("loop")
        self.resume("loop")
        self.kill("loop")
        snap = self.unchanged("loop")
        self.refused("within --not-within 1h0m0s", "resume", "loop", "--not-within", "1h")
        self.assertIn("nothing to do", self.ok("resume", "loop", "--not-within", "1h", "--if-dead"))
        self.assertEqual(self.unchanged("loop"), snap)
        self.refused("takes a duration", "resume", "loop", "--not-within", "soon")
        self.resume("loop", "--not-within", "1ms")
        self.assertEqual(self.record("loop")["resumes"], 2)

    def lock(self, name):
        import fcntl
        tag = self.records(name)[0].name.split(".")[1]
        (self.state / "locks").mkdir(exist_ok=True)
        f = open(self.state / "locks" / f"{name}.{tag}.lock", "w")
        fcntl.flock(f, fcntl.LOCK_EX)  # another resume, dispatch or done, mid-way
        return f

    def state_files(self):
        return sorted(str(p.relative_to(self.state)) for p in self.state.rglob("*") if "locks" not in p.parts)

    def test_one_resume_or_dispatch_of_a_name_at_a_time(self):
        self.dispatch("racy", "--cwd", self.repo)
        self.dispatch("other", "--cwd", self.repo)
        self.kill("racy")
        with self.lock("racy"):
            snap = self.unchanged("racy")
            self.refused("another resume or dispatch of racy is running", "resume", "racy")
            self.assertIn("nothing to do", self.ok("resume", "racy", "--if-dead"))
            self.refused("of racy is running", "dispatch", "racy", "--cwd", self.repo)
            self.assertEqual(self.unchanged("racy"), snap)
            self.kill("other")
            self.resume("other")  # another name isn't held up
        self.resume("racy")
        self.assertIsNotNone(self.agent("racy"))

    def test_a_dispatch_that_fails_after_its_record_says_the_agent_is_up(self):
        rc, out = self.bowery("dispatch", "halfway", "--cwd", self.repo, "--", "hello", env={"FAKE_PROMPT_FAIL": "1"})
        self.assertNotEqual(rc, 0, out)
        self.assertIn("halfway is up in", out)
        self.assertIn("recorded, but its prompt failed", out)
        self.assertIn('"name": "halfway"', out, "its JSON line, so a caller knows where it is")
        self.assertEqual(self.record("halfway")["pane_id"], self.agent("halfway")["pane_id"])
        self.ok("done", "halfway", "--force")

    def test_a_failed_prompt_in_a_reused_pane_is_taken_back_and_said_plainly(self):
        # Its own pane, made by bowery: closed with the half-started claude in it.
        a = self.dispatch("ownp", "--cwd", self.repo)
        self.exit_to_shell("ownp")
        before = self.records("ownp")[0].read_text()
        rc, out = self.bowery("resume", "ownp", env={"FAKE_PROMPT_FAIL": "1"})
        self.assertNotEqual(rc, 0, out)
        self.assertIn("it was taken back", out)
        self.assertIsNone(self.pane(a["pane_id"]))
        self.assertEqual(self.records("ownp")[0].read_text(), before, "the record is put back as it was")
        # A pane that was someone's own: left, unnamed, and said so.
        host = self.new_workspace("host2", str(self.repo))
        self.dispatch("theirs", "--pane", host["pane_id"], "--cwd", self.repo)
        self.exit_to_shell("theirs")
        rc, out = self.bowery("resume", "theirs", env={"FAKE_PROMPT_FAIL": "1"})
        self.assertNotEqual(rc, 0, out)
        self.assertIn(f"still running in {host['pane_id']}, unnamed", out)
        self.assertNotIn("theirs", self.agent_names(), "it doesn't keep the agent's name")
        self.refused("an unnamed claude, perhaps left by a resume that was taken back", "resume", "theirs")

    def test_a_crash_after_the_start_leaves_a_recorded_live_seat(self):
        from _binary import BIN
        self.dispatch("crash", "--cwd", self.repo)
        self.kill("crash")
        proc = subprocess.Popen([BIN, "resume", "crash"], env={**self.env, "FAKE_PROMPT_SLEEP": "8"}, cwd=self.t,
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        import time
        for _ in range(100):
            if self.record("crash").get("resumes") == 1:
                break
            time.sleep(0.1)
        proc.kill()  # between the record and the first prompt
        proc.wait()
        a = self.agent("crash")
        self.assertIsNotNone(a)
        self.assertEqual(self.record("crash")["pane_id"], a["pane_id"], "recorded where it is")
        rc, got = self.json_of("crash", "--json", "--if-dead")
        self.assertEqual(got["outcome"], "alive", "so a job leaves it be")

    def test_an_ordinary_resume_carries_no_warning(self):
        self.dispatch("calm", "--cwd", self.repo)
        self.kill("calm")
        rc, got = self.json_of("calm", "--json")
        self.assertEqual(got["outcome"], "resumed")
        self.assertNotIn("warning", got)

    def test_a_failed_resume_doesnt_wedge_the_next(self):
        self.dispatch("fails", "--cwd", self.repo)
        self.kill("fails")
        rc, _ = self.bowery("resume", "fails", env={"FAKE_PROMPT_FAIL": "1"})
        self.assertNotEqual(rc, 0)
        self.assertIsNone(self.agent("fails"), "taken back")
        self.resume("fails")

    def test_dispatch_wont_replace_a_record(self):
        self.dispatch("kept", "--cwd", self.repo)
        self.kill("kept")
        self.resume("kept")
        self.kill("kept")
        before = self.records("kept")[0].read_text()
        self.refused("bowery resume kept, or bowery done kept first", "dispatch", "kept", "--cwd", self.repo)
        self.assertEqual(self.records("kept")[0].read_text(), before, "its resumes and session are kept")
        self.assertIsNone(self.agent("kept"))

    def test_a_refused_resume_leaves_the_state_folder_as_it_was(self):
        self.dispatch("st", "--cwd", self.repo)
        before = self.state_files()
        self.refused("still alive", "resume", "st")
        self.kill("st")
        self.ok("resume", "st", "--dry-run")
        self.refused("no dispatch record", "resume", "nobody")
        self.assertEqual(self.state_files(), before)
        self.assertFalse(any((self.state / "locks").glob("nobody.*")), "no lock for a name with no record")

    def test_not_within_reads_every_stamp_and_never_hides_a_refusal(self):
        self.dispatch("stamp", "--cwd", self.repo)
        self.kill("stamp")
        f = self.records("stamp")[0]
        rec = json.loads(f.read_text())
        rec["resumed"] = "yesterday-ish"
        f.write_text(json.dumps(rec))
        self.refused("can't read", "resume", "stamp", "--not-within", "10m", "--if-dead")
        rec["resumed"] = "2999-01-01T00:00:00+00:00"  # a clock that ran ahead
        f.write_text(json.dumps(rec))
        self.assertIn("in the future (counted as now)", self.ok("resume", "stamp", "--not-within", "10m", "--if-dead"))
        # Too soon AND its directory gone: the refusal wins, even under --if-dead.
        d = self.t / "gone-dir"
        d.mkdir()
        rec["cwd"] = str(d)
        f.write_text(json.dumps(rec))
        d.rmdir()
        self.refused("which is gone", "resume", "stamp", "--not-within", "10m", "--if-dead")

    # ------------------------------------------------------------ --fresh

    def test_fresh_starts_a_new_conversation_where_it_was(self):
        self.dispatch("anew", "--worktree", "anew", "--cwd", self.repo)
        old = self.record("anew")["session"]
        self.kill("anew")
        self.assertIn("in a new conversation", self.ok("resume", "anew", "--fresh", "--dry-run"))
        self.resume("anew", "--fresh", "--", "3 unread letters")
        a = self.agent("anew")
        self.assertEqual(a["args"], ["--dangerously-skip-permissions"], "no --resume")
        self.assertEqual(self.pane(a["pane_id"])["cwd"], str(self.wt / "repo" / "anew"), "in its own worktree")
        text = a["prompts"][0]["text"]
        self.assertTrue(text.startswith("You were started again in a new conversation"))
        self.assertTrue(text.endswith("\n\n3 unread letters"))
        rec = self.record("anew")
        self.assertNotEqual(rec["session"], old)
        self.assertEqual(rec["session"], a["session"], "the record follows the new conversation")
        self.assertEqual(rec["resumes"], 1, "counted, so --not-within still guards it")

    def test_fresh_needs_no_session_but_refuses_while_the_old_one_lives(self):
        self.dispatch("nosess", "--cwd", self.repo)
        sess = self.record("nosess")["session"]
        self.kill("nosess")
        p = self.new_workspace("elsewhere", str(self.repo))
        self.start_by_hand("copy", p["pane_id"], "--", "--resume", sess)
        self.refused("can't run twice", "resume", "nosess", "--fresh")
        self.herdr("pane", "close", p["pane_id"])
        f = self.records("nosess")[0]
        rec = json.loads(f.read_text())
        rec.pop("session")
        f.write_text(json.dumps(rec))
        self.refused("has no session id", "resume", "nosess")
        self.resume("nosess", "--fresh")
        self.assertTrue(self.record("nosess")["session"])

    def test_fresh_goes_through_the_launcher_and_keeps_the_guards(self):
        seat = self.launcher()
        self.dispatch("fseat", "--cwd", self.repo, "--launcher", f"{seat} fseat")
        self.kill("fseat")
        self.resume("fseat", "--fresh", "--if-dead", "--not-within", "10m")
        self.assertEqual(self.agent("fseat")["env"].get("SEAT_BOX"), "box-fseat")
        self.kill("fseat")
        self.assertIn("nothing to do", self.ok("resume", "fseat", "--fresh", "--if-dead", "--not-within", "10m"))

    def json_of(self, *args, env=None):
        """resume --json, stdout and stderr apart: -> (exit code, the one JSON object)."""
        from _binary import BIN
        p = subprocess.run([BIN, "resume", *map(str, args)], env={**self.env, **(env or {})}, cwd=self.t,
                           capture_output=True, text=True, stdin=subprocess.DEVNULL)
        lines = p.stdout.splitlines()
        self.assertEqual(len(lines), 1, f"exactly one line on stdout, got {p.stdout!r} (stderr {p.stderr!r})")
        return p.returncode, json.loads(lines[0])

    def test_json_is_one_line_with_its_outcome_on_every_path(self):
        self.dispatch("js", "--cwd", self.repo)
        def outcome(*args, rc=0, env=None):
            got_rc, got = self.json_of("js", "--json", *args, env=env)
            self.assertEqual(got_rc == 0, rc == 0, got)
            return got
        self.assertEqual(outcome("--if-dead")["outcome"], "alive")
        self.kill("js")
        self.assertEqual(outcome("--dry-run")["outcome"], "dry_run")
        got = outcome("--if-dead")
        self.assertEqual((got["outcome"], got["name"]), ("resumed", "js"))
        self.assertTrue(got["pane_id"])
        self.kill("js")
        self.assertEqual(outcome("--if-dead", "--not-within", "1h")["outcome"], "too_soon")
        self.assertEqual(outcome("--fresh")["outcome"], "started_fresh")
        self.kill("js")
        with self.lock("js"):
            self.assertEqual(outcome("--if-dead")["outcome"], "busy")
        self.assertEqual(outcome("--not-within", "1h", rc=1)["outcome"], "refused")
        # Its first prompt never arrived: taken back and "failed", for a retry.
        got = outcome(env={"FAKE_PROMPT_FAIL": "1"}, rc=1)
        self.assertEqual(got["outcome"], "failed")
        self.assertIn("first prompt didn't reach it", got["why"])
        self.assertIsNone(self.agent("js"))
        # The name is the first word that isn't an option or its value.
        rc, got = self.json_of("--json", "--not-within", "10m", "js", "--bogus")
        self.assertEqual((got["name"], got["outcome"]), ("js", "refused"))
        # A bad config still answers with one line.
        self.config.write_text("this is = = not toml\n")
        rc, got = self.json_of("js", "--json")
        self.assertNotEqual(rc, 0)
        self.assertEqual((got["name"], got["outcome"]), ("js", "refused"))
        self.config.write_text("")
        # Usage errors still keep the promise.
        for bad in (["--frobnicate"], ["--not-within", "soon"], ["--swarm", "--workspace"], ["other-name"]):
            rc, got = self.json_of("js", "--json", *bad)
            self.assertNotEqual(rc, 0)
            self.assertEqual((got["outcome"], got["name"]), ("refused", "js"), bad)
        rc, got = self.json_of("--json")
        self.assertEqual(got["outcome"], "refused")
        # A bad first word is the name refused, not a later word or nothing.
        rc, got = self.json_of("Foo", "--json")
        self.assertEqual((got["name"], got["outcome"]), ("Foo", "refused"))
        rc, got = self.json_of("Foo", "js", "--json")
        self.assertEqual((got["name"], got["outcome"]), ("Foo", "refused"))

    def test_fresh_refuses_a_launcher_that_resumes(self):
        self.dispatch("fr", "--cwd", self.repo)
        sess = self.record("fr")["session"]
        self.kill("fr")
        sneaky = self.launcher("sneaky", f'exec claude --resume {sess} "$@"')
        rc, got = self.json_of("fr", "--fresh", "--json", "--launcher", str(sneaky))
        self.assertNotEqual(rc, 0, got)
        self.assertEqual(got["outcome"], "failed")
        self.assertIn(f"came back on its old conversation {sess}", got["why"])
        self.assertIn("taken back", got["why"])
        self.assertEqual(self.record("fr")["session"], sess, "the record keeps it")
        self.assertIsNone(self.agent("fr"))

    def test_fresh_starts_an_old_record(self):
        self.dispatch("ancient", "--cwd", self.repo)
        f = self.records("ancient")[0]
        rec = json.loads(f.read_text())
        for k in ("session", "cwd", "gated", "model", "terminal_id", "owns"):
            rec.pop(k, None)
        rec["worktree"], rec["branch"] = str(self.repo), None
        f.write_text(json.dumps(rec))
        self.kill_by_tab(rec["tab_id"])
        self.assertIn("bowery resume ancient --fresh --gated|--ungated", self.ok("orphans"))
        self.refused("--fresh starts it in a new conversation", "resume", "ancient")
        self.resume("ancient", "--fresh", "--ungated")
        self.assertTrue(self.record("ancient")["session"])

    def kill_by_tab(self, tab):
        self.herdr("tab", "close", tab)

    # ------------------------------------------------------------ adopt a live seat

    def test_adopt_gives_a_hand_started_agent_a_record_resume_can_use(self):
        seat = self.launcher()
        p = self.new_workspace("byhand", str(self.repo))
        self.start_by_hand("handy", p["pane_id"])
        sess = self.agent("handy")["session"]
        self.refused("--gated (permission prompts on) or --ungated", "adopt", "handy", "--launcher", f"{seat} handy")
        self.assertEqual(self.records("handy"), [], "nothing written")
        self.refused("nothing to adopt", "adopt", "handy")
        self.ok("adopt", "handy", "--launcher", f"{seat} handy", "--ungated")
        rec = self.record("handy")
        self.assertEqual((rec["session"], rec["cwd"], rec["launcher"]), (sess, str(self.repo), f"{seat} handy"))
        self.assertNotIn("gated", rec, "ungated")
        self.assertIsNotNone(self.agent("handy"), "nothing restarted")
        self.exit_to_shell("handy")
        self.resume("handy")
        a = self.agent("handy")
        self.assertEqual(a["env"].get("SEAT_BOX"), "box-handy", "back through the adopted launcher")
        self.assertEqual(a["args"], ["--dangerously-skip-permissions", "--resume", sess])

    def test_adopt_checks_everything_before_it_writes(self):
        p = self.new_workspace("hands", str(self.repo))
        self.start_by_hand("twin", p["pane_id"])
        with self.mutate() as s:  # herdr reports no session for it
            s["agents"]["twin"].pop("session")
        out = self.refused("herdr reports no session", "adopt", "twin", "--ungated")
        self.assertNotIn("ungated", out.split("refused")[0], "nothing said before the refusal")
        self.assertEqual(self.records("twin"), [])
        with self.mutate() as s:
            s["agents"]["twin"]["session"] = "s-1"
        q = self.new_workspace("hands2", str(self.repo))
        self.start_by_hand("~", q["pane_id"])  # a second agent...
        with self.mutate() as s:
            a = s["agents"].pop("~")
            a["name"] = "twin"
            s["agents"]["twin-2"] = a  # ...also named twin
        self.refused("more than one live agent is named twin", "adopt", "twin", "--ungated")
        self.assertEqual(self.records("twin"), [])
        self.refused("must be lowercase", "adopt", "../../x", "--ungated")
        self.assertFalse(any(self.state.rglob("x.*")), "no lock file outside locks/")

    def test_adopt_records_the_folder_claude_runs_in_not_the_shells(self):
        p = self.new_workspace("cwdtest", str(self.repo))
        self.start_by_hand("where", p["pane_id"])
        with self.mutate() as s:  # the shell moved on; claude still runs in repo
            next(x for x in s["panes"] if x["pane_id"] == p["pane_id"])["cwd"] = str(self.t)
        self.ok("adopt", "where", "--ungated")
        self.assertEqual(self.record("where")["cwd"], str(self.repo))

    def test_adopt_wont_fill_in_an_old_record(self):
        self.dispatch("elder", "--cwd", self.repo)
        f = self.records("elder")[0]
        rec = json.loads(f.read_text())
        for k in ("session", "cwd", "gated", "model"):
            rec.pop(k, None)
        f.write_text(json.dumps(rec))
        before = f.read_text()
        self.refused("which adopt doesn't fill in; bowery done elder", "adopt", "elder", "--ungated")
        self.assertEqual(f.read_text(), before, "nothing written")
        rec["cwd"] = str(self.repo)  # a folder, but no session
        f.write_text(json.dumps(rec))
        self.refused("its record has no session, which adopt doesn't fill in", "adopt", "elder", "--ungated")

    def test_adopt_takes_paths_after_double_dash(self):
        self.dispatch("dd", "--cwd", self.repo)
        w = self.wt / "repo" / "--odd"
        sh("git", "-C", self.repo, "worktree", "add", "-q", "-b", "odd", str(w), "origin/main")
        self.ok("adopt", "dd", "--", str(w))
        self.assertIn(str(w), [x["path"] for x in self.record("dd")["worktrees"]])
        # --machine after -- is a path, not the option.
        self.refused("adopt:", "adopt", "dd", "--", "--machine")

    def test_adopt_sets_a_launcher_on_an_existing_record(self):
        seat = self.launcher()
        self.dispatch("plainrec", "--cwd", self.repo)
        self.ok("adopt", "plainrec", "--launcher", f"{seat} plainrec")
        self.assertEqual(self.record("plainrec")["launcher"], f"{seat} plainrec")
        self.refused("not the option", "adopt", "plainrec", "--launcher", "--gated")

    def test_an_old_herdr_without_sessions_is_named_as_the_cause(self):
        old = {"FAKE_NO_SESSIONS": "1"}
        rc, out = self.bowery("dispatch", "oldh", "--cwd", self.repo, env=old)
        self.assertEqual(rc, 0, out)
        self.assertIn("needs 0.9.5 or later", out)
        p = self.new_workspace("oldhand", str(self.repo))
        self.start_by_hand("byhand2", p["pane_id"])
        self.refused("needs 0.9.5 or later", "adopt", "byhand2", "--ungated", env=old)
        rc, out = self.bowery("dispatch", "newh", "--cwd", self.repo)
        self.assertNotIn("0.9.5", out, "a herdr that reports sessions isn't blamed")

    # ------------------------------------------------------------ orphans and list

    def test_orphans_shows_an_agent_that_quit_to_its_shell(self):
        self.dispatch("quit2", "--cwd", self.repo)
        self.exit_to_shell("quit2")
        out = self.ok("orphans")
        self.assertIn("quit2: agent gone, its pane still open  (bowery resume quit2; done ends that)", out)
        self.assertRegex(self.ok("list"), r"gone\s+quit2\s+bowery resume quit2")

    def test_orphans_and_list_say_who_can_be_resumed(self):
        self.dispatch("idle1", "--cwd", self.repo)
        self.dispatch("dead", "--worktree", "dead", "--cwd", self.repo)
        self.kill("dead")
        out = self.ok("orphans")
        self.assertIn("finished dispatched agents still alive (done <name>):", out, "the heading other tools parse")
        self.assertRegex(out, r"\n  idle1  idle  \S+  \S+", "the agent line's shape is kept")
        self.assertIn("dead: agent gone, its worktree still there  (bowery resume dead; done ends that)", out)
        self.assertIn("now gone; bowery resume dead can still use it, done ends that)", out)
        listed = self.ok("list")
        self.assertRegex(listed, r"gone\s+dead\s+bowery resume dead")

        js = {(o["kind"], o["target"]): o for o in json.loads(self.ok("orphans", "--json"))}
        self.assertTrue(js[("record", "dead")]["resumable"])
        self.assertEqual(js[("record", "dead")]["resumes"], 0)
        self.assertEqual(js[("agent", "idle1")]["status"], "idle")
        wt = js[("worktree", str(self.wt / "repo" / "dead"))]
        self.assertEqual((wt["branch"], wt["merged"], wt["name"]), ("dead", True, "dead"), "no commits yet: nothing to lose")

        self.resume("dead")
        out = self.ok("orphans")
        self.assertNotIn("dead:", out, "a resumed agent is alive: no longer a dead record")
        self.assertEqual(out.count("dead  idle"), 1, "listed once, as a live agent")
        js = json.loads(self.ok("orphans", "--json"))
        self.assertEqual([o["resumes"] for o in js if o["kind"] == "agent" and o["name"] == "dead"], [1])

    def test_orphans_json_with_nothing_is_an_empty_list(self):
        self.assertEqual(json.loads(self.ok("orphans", "--json")), [])


class RemoteResume(Resume.__base__):
    def setUp(self):
        super().setUp()
        self.add_box()
        self.app, self.app_other = self.box_repo()

    def far(self):
        self.dispatch("far", "--machine", BOX, "--as", "alice", "--worktree", "far", "--cwd", "~/dev/app")
        rec = json.loads(self.records("far")[0].read_text())
        self.herdr("tab", "close", rec["tab_id"], machine=BOX)
        return rec

    def test_resumes_on_its_machine_as_its_identity(self):
        rec = self.far()
        open(self.box / "as.log", "w").close()
        out = self.ok("resume", "far")  # found by its record, as done finds it
        res = json.loads(out.strip().splitlines()[-1])
        self.assertEqual((res["machine"], res["as"]), (BOX, "alice"))
        a = self.agent("far", BOX)
        self.assertEqual(a["env"].get("BOWERY_AS"), "alice", "the pane became alice before the agent started")
        self.assertEqual(a["args"], ["--dangerously-skip-permissions", "--resume", rec["session"]])
        self.assertEqual(self.pane(a["pane_id"], BOX)["cwd"], rec["worktree"])
        self.assertTrue(a["prompts"][0]["text"].startswith(NOTICE))
        self.assertIn(f"{rec['worktree']} is on far at", a["prompts"][0]["text"], "read on the box")
        self.assertEqual(self.ws_labels(), [], "nothing on the local server")
        self.assertEqual(json.loads(self.records("far")[0].read_text())["resumes"], 1)
        self.ok("done", "far", "--force")
        self.assertIsNone(self.agent("far", BOX))

    def test_unreachable_box_is_marked_in_orphans_json(self):
        self.far()
        self.env["FAKE_SSH_HOSTS"] = ""
        out = self.ok("orphans", "--json")
        js, _ = json.JSONDecoder().raw_decode(out)
        self.assertEqual([(o["kind"], o["machine"]) for o in js], [("unreachable", BOX)])
        self.assertIn("unreachable", out[out.index("]"):], "and a warning")

    def test_refused_when_its_identity_command_is_gone(self):
        self.far()
        self.config.write_text(f"[machines.{BOX}]\n")
        self.refused("no identity_command", "resume", "far")
        self.assertIsNone(self.agent("far", BOX))

    def test_checks_on_the_box_run_as_its_identity(self):
        self.far()
        open(self.box / "as.log", "w").close()
        self.ok("resume", "far", "--dry-run")
        self.assertIn("test -d", (self.box / "as.log").read_text(), "the directory test ran as alice")

    def test_a_launcher_on_the_box_runs_after_the_identity(self):
        seat = self.t / "seat-launch"
        seat.write_text('#!/bin/bash\nexport SEAT_BOX=box-"$1"; shift; exec claude "$@"\n')
        seat.chmod(0o755)
        self.dispatch("farseat", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app", "--launcher", f"{seat} farseat")
        a = self.agent("farseat", BOX)
        self.assertEqual((a["env"].get("BOWERY_AS"), a["env"].get("SEAT_BOX")), ("alice", "box-farseat"))
        rec = json.loads(self.records("farseat")[0].read_text())
        self.herdr("tab", "close", rec["tab_id"], machine=BOX)
        self.ok("resume", "farseat")
        a = self.agent("farseat", BOX)
        self.assertEqual((a["env"].get("BOWERY_AS"), a["env"].get("SEAT_BOX")), ("alice", "box-farseat"))
        self.assertEqual(a["args"], ["--dangerously-skip-permissions", "--resume", rec["session"]])

    def test_refusals_on_the_box(self):
        self.dispatch("near", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app")
        self.refused("still alive", "resume", "near", "--machine", BOX)
        rec = self.far()
        shutil.rmtree(rec["worktree"])
        self.refused("is gone", "resume", "far")
        self.assertIsNone(self.agent("far", BOX))

    def test_another_tools_marker_is_read_on_the_box(self):
        self.add_box(extra='')
        self.config.write_text(self.config.read_text().replace(f"[machines.{BOX}]",
                               f'hands_off_env = ["OTHER_TOOL_TAG"]\n[machines.{BOX}]'))
        self.dispatch("bx", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app")
        rec = json.loads(self.records("bx")[0].read_text())
        self.exit_to_shell("bx", BOX)
        proc = subprocess.Popen(["sleep", "300"], env={**os.environ, "OTHER_TOOL_TAG": "x-1"})
        self.addCleanup(proc.wait)
        self.addCleanup(proc.kill)
        with self.mutate(BOX) as s:
            next(p for p in s["panes"] if p["pane_id"] == rec["pane_id"])["pid"] = proc.pid
        self.refused("carrying OTHER_TOOL_TAG", "resume", "bx")
        self.assertIn("/proc/", self.ssh_log(), "read over ssh")

    exit_to_shell = Resume.exit_to_shell


if __name__ == "__main__":
    unittest.main()
