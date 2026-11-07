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
        for k in ("session", "cwd", "gated", "model"):
            rec.pop(k, None)
        f.write_text(json.dumps(rec))
        self.kill("old")
        self.refused("has no session id", "resume", "old")
        self.ok("list")
        self.ok("orphans")
        self.assertNotIn("session", json.loads(f.read_text()), "nothing invented")
        out = json.loads(self.ok("orphans", "--json"))
        self.assertEqual([o["resumable"] for o in out if o["name"] == "old"], [False])

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

    def test_a_failure_after_the_start_still_records_where_the_agent_is(self):
        self.dispatch("late", "--cwd", self.repo)
        self.kill("late")
        rc, out = self.bowery("resume", "late", env={"FAKE_PROMPT_FAIL": "1"})
        self.assertNotEqual(rc, 0)
        self.assertIn("is running again in", out)
        a = self.agent("late")
        rec = self.record("late")
        self.assertEqual((rec["pane_id"], rec["terminal_id"]), (a["pane_id"], self.pane(a["pane_id"])["terminal_id"]))
        self.assertEqual(rec["resumes"], 1)
        self.ok("done", "late", "--force")
        self.assertEqual(self.swarm(), None, "done owns the new tab: nothing left")

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
        # Where ps -E stands in for /proc (a Mac), another user's process
        # comes back as its command alone, exit 0: that's no read either.
        bin_ = self.t / "macps"
        bin_.mkdir()
        (bin_ / "ps").write_text("#!/bin/sh\necho /sbin/launchd\n")
        (bin_ / "ps").chmod(0o755)
        with self.mutate() as s:
            next(p for p in s["panes"] if p["pane_id"] == a["pane_id"])["pid"] = 1
        self.refused("can't read the environment of pid 1", "resume", "unread",
                     env={"PATH": f"{bin_}:{self.env['PATH']}"})
        (bin_ / "ps").write_text("#!/bin/sh\necho /bin/zsh -l HOME=/home/x PATH=/usr/bin OTHER_TOOL_TAG=1\n")
        if not os.access("/proc/1/environ", os.R_OK):
            self.refused("carrying OTHER_TOOL_TAG", "resume", "unread", env={"PATH": f"{bin_}:{self.env['PATH']}"})
        self.assertEqual(self.unchanged("unread"), snap)

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
