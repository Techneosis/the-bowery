"""dispatch and done, adopt, orphans, list on the local server.

The original Python tool's self-test, case for case, against the fake herdr
(tests/_fake.py) instead of a live server with real agents. Each test is one
of its sections; the comments name what the fake stands in for.

    python3 -m unittest discover tests
"""

import json
import unittest
from pathlib import Path

from _fake import FAKES, Bed, sh


class Start(Bed):
    def test_worktree_in_the_default_swarm_placement(self):
        out = self.dispatch("wt-merged", "--worktree", "wt-merged", "--cwd", self.repo, env={"BOWERY_TRUST_ROOTS": ""})
        wt = self.wt / "repo" / "wt-merged"
        self.assertEqual(out["worktree"], str(wt))
        self.assertEqual(out["branch"], "wt-merged")
        self.assertIn("wt-merged", self.agent_names(), "agent is up under its name")
        a = self.agent("wt-merged")
        self.assertEqual(self.pane(a["pane_id"])["tokens"], {"sound": "off"}, "tagged sound=off")
        self.assertEqual(self.pane(a["pane_id"])["token_source"], "bowery.sound")
        self.assertEqual(a["agent_status"], "idle", "folder-trust dialog answered (worktree root is trusted)")
        self.assertEqual(len(self.records("wt-merged")), 1, "dispatch record written")
        self.assertEqual(sh("git", "-C", wt, "rev-parse", "HEAD"), sh("git", "-C", self.repo, "rev-parse", "origin/main"),
                         "branched from origin/main")
        sw = self.swarm()
        self.assertIsNotNone(sw, "a swarm workspace was made")
        self.assertNotIn("worktree", sw, "swarm is not a herdr worktree group")
        self.assertEqual(self.tab_labels(sw["workspace_id"]), ["wt-merged"], "own tab, and swarm's blank tab is gone")
        self.assertNotIn("repo", self.ws_labels(), "no repo-root primary spawned")
        self.assertEqual(sw["cwd"], str(self.home), "swarm is rooted at $HOME")

    def test_start_output_and_record(self):
        out = self.dispatch("plain", "--cwd", self.repo)
        self.assertEqual(set(out), {"name", "pane_id", "workspace_id", "tab_id", "sound"})
        rec = self.record("plain")
        self.assertEqual(rec["owns"], {"kind": "tab", "id": out["tab_id"]})
        self.assertEqual(rec["placement"], "swarm")
        self.assertEqual(rec["socket"], self.env["HERDR_SOCKET_PATH"])
        self.assertTrue(rec["created_pane"])
        self.assertNotIn("machine", rec)

    def test_bypass_by_default_gated_and_model(self):
        self.dispatch("byp", "--cwd", self.repo)
        self.assertEqual(self.agent("byp")["args"], ["--dangerously-skip-permissions"])
        self.dispatch("gat", "--gated", "--model", "haiku", "--cwd", self.repo)
        self.assertEqual(self.agent("gat")["args"], ["--model", "haiku"], "aliases pass through unexpanded")
        self.config.write_text('[models]\nfast = "claude-haiku-4-5"\n')
        self.dispatch("mdl", "--model", "fast", "--cwd", self.repo)
        self.assertEqual(self.agent("mdl")["args"], ["--dangerously-skip-permissions", "--model", "claude-haiku-4-5"])
        self.refused("only supported with --kind claude", "dispatch", "cx", "--kind", "codex", "--model", "x")
        self.dispatch("cdx", "--kind", "codex", "--cwd", self.repo)
        self.assertEqual(self.agent("cdx")["args"], [], "other kinds get no permissions flag")

    def test_prompt_and_wait(self):
        self.dispatch("pr", "--wait", "--cwd", self.repo, "--", "do", "the", "thing")
        self.assertEqual(self.agent("pr")["prompts"], [{"text": "do the thing", "wait": True}])

    def test_loud_and_mute(self):
        out = self.dispatch("noisy", "--loud", "--cwd", self.repo)
        self.assertEqual(out["sound"], "on")
        self.assertEqual(self.pane(out["pane_id"])["tokens"], {})
        self.assertIn("noisy (", self.ok("mute", "noisy"))
        self.assertEqual(self.pane(out["pane_id"])["tokens"], {"sound": "off"})
        listed = self.ok("list")
        self.assertRegex(listed, rf"{out['pane_id']}\s+off\s+idle\s+noisy")
        self.ok("unmute", out["pane_id"])
        self.assertEqual(self.pane(out["pane_id"])["tokens"], {})
        self.refused("no agent or pane named ghost", "mute", "ghost")

    def test_sound_source_is_configurable(self):
        self.config.write_text('sound_source = "acme.sound"\n')
        out = self.dispatch("src", "--cwd", self.repo)
        self.assertEqual(self.pane(out["pane_id"])["token_source"], "acme.sound")

    def test_swarm_name_is_configurable(self):
        self.config.write_text('swarm = "agents"\n')
        self.dispatch("named", "--cwd", self.repo)
        self.assertEqual(self.ws_labels(), ["agents"])
        self.ok("done", "named")
        self.assertEqual(self.ws_labels(), [])

    def test_placements(self):
        host = self.new_workspace("host", self.repo)
        sp = self.dispatch("sp", "--split", "down", "--from", host["pane_id"], "--cwd", self.repo)
        self.assertEqual(sp["workspace_id"], host["workspace_id"])
        self.assertNotIn("owns", self.record("sp"))
        tb = self.dispatch("tb", "--tab", "--from", host["pane_id"], "--cwd", self.repo)
        self.assertEqual(tb["workspace_id"], host["workspace_id"])
        self.assertEqual(self.record("tb")["owns"], {"kind": "tab", "id": tb["tab_id"]})
        wsp = self.dispatch("wsp", "--workspace", "--cwd", self.repo)
        self.assertIn("wsp", self.ws_labels())
        self.assertEqual(self.record("wsp")["owns"], {"kind": "workspace", "id": wsp["workspace_id"]})
        blank = self.split_off(host["pane_id"])
        pn = self.dispatch("pn", "--pane", blank["pane_id"])
        self.assertEqual(pn["pane_id"], blank["pane_id"])
        self.assertFalse(self.record("pn")["created_pane"])

    def test_tab_and_split_need_a_pane_to_start_from(self):
        # Outside any pane, `herdr pane current` names the focused one, as
        # the real server does; with no panes at all there is none.
        self.refused("not inside a herdr pane", "dispatch", "lost", "--split")

    def test_names_and_argument_errors(self):
        self.refused("agent name must be lowercase", "dispatch", "Bad Name")
        self.refused("--base only goes with --worktree", "dispatch", "x", "--base", "main")
        self.refused("drop --pane", "dispatch", "x", "--worktree", "b", "--pane", "w1:p1")
        self.refused("unknown option --nope", "dispatch", "x", "--nope")
        self.refused("--also-worktree: ", "dispatch", "x", "--also-worktree", self.repo)
        self.refused("bowery dispatch x", "x")
        self.refused("--worktree: branch main already exists", "dispatch", "x", "--worktree", "main", "--cwd", self.repo)
        self.refused("not a valid branch name", "dispatch", "x", "--worktree", "a..b", "--cwd", self.repo)
        self.refused("is not in a git repository", "dispatch", "x", "--worktree", "b", "--cwd", self.home)
        self.assertEqual(self.ws_labels(), [], "none of that made a workspace")
        self.assertEqual(self.records(), [], "nor a record")

    def test_base_is_used_as_given(self):
        first = sh("git", "-C", self.repo, "rev-parse", "HEAD")
        self.commit_in(self.repo, "later")
        out = self.dispatch("bs", "--worktree", "bs", "--base", first, "--cwd", self.repo)
        self.assertEqual(sh("git", "-C", out["worktree"], "rev-parse", "HEAD"), first)
        self.assertEqual(self.record("bs")["base"], first)

    def test_a_shell_not_yet_ready_is_retried(self):
        out = self.dispatch("slow", "--cwd", self.repo, env={"FAKE_PANE_WARMUP": "3"})
        self.assertIn("slow", self.agent_names())
        starts = [c for c in self.state_of()["log"] if c[:2] == ["agent", "start"]]
        self.assertEqual(len(starts), 4, "three agent_pane_busy, then the start")
        self.assertEqual(out["name"], "slow")

    def test_a_branch_with_a_slash_gets_herdrs_dash_named_checkout(self):
        self.dispatch("wt-slash", "--worktree", "feat/slashy", "--cwd", self.repo)
        self.assertTrue((self.wt / "repo" / "feat-slashy").is_dir())
        self.ok("done", "wt-slash")
        self.assertNotIn("feat/slashy", self.branches())


class StartFailure(Bed):
    def test_trust_dialog_outside_the_trusted_roots_leaves_nothing_behind(self):
        untrusted = self.t / "untrusted"
        untrusted.mkdir()
        rc, out = self.bowery("dispatch", "nope", "--workspace", "--cwd", untrusted,
                              env={"BOWERY_TRUST_ROOTS": "", "BOWERY_WORKTREE_ROOT": str(self.t / "elsewhere")})
        self.assertNotEqual(rc, 0)
        self.assertIn("outside", out)
        self.assertNotIn("nope", self.ws_labels(), "its workspace was closed")
        self.assertEqual(self.records(), [])

    def test_a_failed_worktree_start_removes_the_worktree_and_branch(self):
        rc, out = self.bowery("dispatch", "nope", "--worktree", "doomed", "--cwd", self.repo,
                              env={"FAKE_TRUST_STUCK": "1"})
        self.assertNotEqual(rc, 0, out)
        self.assertFalse((self.wt / "repo" / "doomed").exists())
        self.assertNotIn("doomed", self.branches())
        self.assertIsNone(self.swarm(), "swarm went with its only tab")

    def test_trusted_roots_come_from_the_config(self):
        mine = self.home / "dev" / "proj"
        mine.mkdir(parents=True)
        self.dispatch("home-dev", "--workspace", "--cwd", mine, env={"BOWERY_TRUST_ROOTS": ""})
        other = self.t / "srv"
        other.mkdir()
        self.refused("outside", "dispatch", "srv1", "--workspace", "--cwd", other, env={"BOWERY_TRUST_ROOTS": ""})
        self.config.write_text(f'trust_roots = ["{other}"]\n')
        self.dispatch("srv2", "--workspace", "--cwd", other, env={"BOWERY_TRUST_ROOTS": ""})

    def test_a_dialog_that_will_not_go_away_is_a_failure(self):
        self.refused("still showing", "dispatch", "stuck", "--cwd", self.repo, env={"FAKE_TRUST_STUCK": "1"})
        self.assertEqual(self.ws_labels(), [])


class Done(Bed):
    def wt_agent(self, name):
        self.dispatch(name, "--worktree", name, "--cwd", self.repo)
        return self.wt / "repo" / name

    def test_merged_branch_takes_pane_worktree_branch_and_fast_forwards_main(self):
        wt = self.wt_agent("wt-merged")
        self.commit_in(wt, "a.txt")
        self.merge_pr("wt-merged")
        out = self.ok("done", "wt-merged")
        self.assertIn("closed tab", out)
        self.assertNotIn("wt-merged", self.agent_names())
        self.assertFalse(wt.exists(), "worktree removed")
        self.assertNotIn("wt-merged", self.branches(), "branch deleted")
        self.assertIn("deleted branch wt-merged (merged origin/main)", out)
        self.assertEqual(sh("git", "-C", self.repo, "rev-parse", "HEAD"), sh("git", "-C", self.repo, "rev-parse", "origin/main"),
                         "main checkout fast-forwarded")
        self.assertIn("fast-forwarded", out)
        self.assertEqual(self.records("wt-merged"), [], "record dropped")
        out = self.ok("done", "wt-merged")
        self.assertIn("nothing to tear down", out, "a re-run is a no-op success")

    def test_squash_merge_counts_and_dirty_main_is_not_fast_forwarded(self):
        wt = self.wt_agent("wt-squash")
        self.commit_in(wt, "b.txt")
        self.commit_in(wt, "c.txt")
        self.merge_pr("wt-squash", "squash")
        with open(self.repo / "file", "a") as f:
            f.write("local-edit\n")
        out = self.ok("done", "wt-squash")
        self.assertIn("content already in origin/main", out)
        self.assertNotIn("wt-squash", self.branches())
        self.assertIn("not fast-forwarded: uncommitted changes", out)

    def test_unmerged_refuses_and_force_removes_the_checkout_but_keeps_the_branch(self):
        wt = self.wt_agent("wt-unmerged")
        self.commit_in(wt, "d.txt")
        out = self.refused("is not merged into origin/main", "done", "wt-unmerged")
        self.assertIn("refused, nothing changed", out)
        self.assertIn("wt-unmerged", self.agent_names(), "agent untouched")
        out = self.ok("done", "wt-unmerged", "--force")
        self.assertFalse(wt.exists())
        self.assertIn("wt-unmerged", self.branches(), "unmerged branch kept")
        self.assertIn("kept branch wt-unmerged", out)

    def test_dirty_refuses_keep_worktree_closes_only_the_pane_and_done_path_finishes(self):
        wt = self.wt_agent("wt-dirty")
        (wt / "scratch").touch()
        self.refused("uncommitted or untracked", "done", "wt-dirty")
        self.ok("done", "wt-dirty", "--keep-worktree")
        self.assertNotIn("wt-dirty", self.agent_names(), "pane closed")
        self.assertTrue((wt / "scratch").exists(), "worktree kept")
        self.assertIn(f"{wt}  [wt-dirty, dirty]", self.ok("orphans"), "orphans lists the paneless worktree")
        (wt / "scratch").unlink()
        self.ok("done", wt)
        self.assertFalse(wt.exists())

    def test_dry_run_changes_nothing(self):
        wt = self.wt_agent("dry")
        out = self.ok("done", "dry", "--dry-run")
        self.assertIn("[dry-run] dry: closed tab", out)
        self.assertIn(f"[dry-run] dry: removed worktree {wt}", out)
        self.assertTrue(wt.exists())
        self.assertIn("dry", self.agent_names())
        self.assertEqual(len(self.records("dry")), 1)

    def test_keep_branch(self):
        self.wt_agent("kb")
        self.ok("done", "kb", "--keep-branch")
        self.assertIn("kb", self.branches())

    def test_done_by_pane_id_and_several_targets(self):
        a = self.dispatch("ta", "--cwd", self.repo)
        self.dispatch("tb", "--cwd", self.repo)
        self.ok("done", a["pane_id"], "tb")
        self.assertEqual(self.agent_names(), [])
        self.assertEqual(self.records(), [])

    def test_unknown_option_and_no_target(self):
        self.refused("done: unknown option --nope", "done", "x", "--nope")
        self.refused("done: which agent?", "done")

    def test_a_working_agent_is_refused_and_idle_agents_show_as_orphans(self):
        self.dispatch("busy", "--workspace", "--cwd", self.repo, "--", "sleep 25 then reply")
        self.set_status("busy", "working")  # the fake's stand-in for a real agent mid-task
        self.refused("still working", "done", "busy")
        self.set_status("busy", "blocked")
        self.refused("herdr agent read", "done", "busy")
        self.set_status("busy", "idle")
        self.assertIn("busy", self.ok("orphans"), "orphans lists the finished agent")
        self.ok("done", "busy")
        self.assertNotIn("busy", self.ws_labels())

    def test_force_overrides_working(self):
        self.dispatch("fw", "--cwd", self.repo)
        self.set_status("fw", "working")
        self.ok("done", "fw", "--force")
        self.assertEqual(self.agent_names(), [])

    def test_done_refuses_to_close_the_pane_it_runs_in(self):
        a = self.dispatch("self", "--cwd", self.repo)
        self.refused("the pane you are running in", "done", "self", env={"HERDR_PANE_ID": a["pane_id"]})
        self.assertIn("self", self.agent_names())

    def test_an_agent_that_exited_to_a_shell_is_found_by_its_terminal(self):
        a = self.dispatch("exited", "--cwd", self.repo)
        with self.mutate() as s:
            del s["agents"]["exited"]
        self.ok("done", "exited")
        self.assertIsNone(self.pane(a["pane_id"]))
        self.assertEqual(self.records(), [])


class Swarm(Bed):
    def test_closing_one_agents_tab_never_touches_another_and_swarm_goes_with_its_last(self):
        self.dispatch("sw-a", "--cwd", self.repo)
        self.dispatch("sw-b", "--worktree", "sw-b", "--cwd", self.repo)
        sw = self.swarm()
        self.assertEqual(len(sw["tabs"]), 2, "both agents share one swarm")
        self.ok("done", "sw-a")
        self.assertIn("sw-b", self.agent_names())
        self.assertEqual(self.agent("sw-b")["agent_status"], "idle")
        self.assertEqual(self.tab_labels(sw["workspace_id"]), ["sw-b"], "sw-b's tab intact, sw-a's gone")
        out = self.ok("done", "sw-b")
        self.assertIn("and its workspace", out)
        self.assertIsNone(self.swarm(), "swarm closed with its last tab")
        self.assertFalse((self.wt / "repo" / "sw-b").exists())

    def test_done_closes_every_pane_an_agent_opened_in_its_tab(self):
        self.dispatch("keep", "--cwd", self.repo)
        a = self.dispatch("sp-a", "--cwd", self.repo)
        extra = self.split_off(a["pane_id"])
        out = self.ok("done", "sp-a")
        self.assertIn("with 2 pane(s)", out)
        self.assertNotIn(extra["terminal_id"], self.terminals(), "the extra blank shell is gone too")
        sw = self.swarm()
        self.assertEqual(self.tab_labels(sw["workspace_id"]), ["keep"], "the other agent's tab is untouched")
        self.assertEqual(self.agent("keep")["agent_status"], "idle")

    def test_a_pane_in_the_tab_running_something_needs_force(self):
        b = self.dispatch("sp-b", "--cwd", self.repo)
        live = self.split_off(b["pane_id"])
        self.run_in(live["pane_id"], "sleep 301")
        out = self.refused("not an idle shell prompt", "done", "sp-b")
        self.assertIn(live["pane_id"], out, "names the busy pane")
        self.assertIn("sp-b", self.agent_names())
        self.assertIsNotNone(self.pane(live["pane_id"]), "its process untouched")
        self.ok("done", "sp-b", "--force")
        self.assertIsNone(self.pane(live["pane_id"]), "the process went with the tab")

    def test_an_agent_gone_from_its_tab_leaves_the_tab_as_an_orphan(self):
        self.dispatch("keep", "--cwd", self.repo)
        c = self.dispatch("sp-c", "--cwd", self.repo)
        stray = self.split_off(c["pane_id"])
        self.herdr("pane", "close", c["pane_id"])
        self.assertIn("sp-c: agent gone, its tab", self.ok("orphans"))
        self.ok("done", "sp-c")
        self.assertNotIn(stray["terminal_id"], self.terminals())
        self.assertIn("keep", self.agent_names())

    def test_a_renamed_tab_is_not_taken_for_the_agents(self):
        c = self.dispatch("ren", "--cwd", self.repo)
        stray = self.split_off(c["pane_id"])
        with self.mutate() as s:
            s["tabs"][c["tab_id"]]["label"] = "mine-now"
        self.ok("done", "ren")
        self.assertIsNotNone(self.pane(stray["pane_id"]), "only the agent's own pane closed")

    def test_workspace_placement_takes_its_extra_panes(self):
        a = self.dispatch("ws-a", "--workspace", "--cwd", self.repo)
        self.split_off(a["pane_id"])
        self.assertIn("with 2 pane(s)", self.ok("done", "ws-a"))
        self.assertNotIn("ws-a", self.ws_labels())

    def test_split_beside_a_host_closes_only_the_agents_pane(self):
        host = self.new_workspace("host", self.repo)
        self.dispatch("wt-split", "--split", "down", "--from", host["pane_id"], "--worktree", "wt-split", "--cwd", self.repo)
        self.ok("done", "wt-split")
        self.assertIn("host", self.ws_labels())
        self.assertFalse((self.wt / "repo" / "wt-split").exists())

    def test_panes_split_off_a_split_agent_are_not_its_own(self):
        host = self.new_workspace("host2", self.repo)
        a = self.dispatch("sp-split", "--split", "down", "--from", host["pane_id"], "--cwd", self.repo)
        nbr = self.split_off(a["pane_id"])
        self.ok("done", "sp-split")
        self.assertIn(host["terminal_id"], self.terminals(), "host pane survived")
        self.assertIn(nbr["terminal_id"], self.terminals(), "the pane split off the agent survived too")

    def test_swarm_is_moved_last_once_made(self):
        self.new_workspace("first", self.repo)
        self.dispatch("sl-a", "--cwd", self.repo)
        self.new_workspace("after", self.repo)
        # A plugin of your own keeps it last as workspaces come and go (see
        # docs/design.md); bowery puts it there whenever it dispatches.
        self.dispatch("sl-b", "--cwd", self.repo)
        self.assertEqual(self.ws_labels()[-1], "swarm")
        self.assertEqual(self.ws_labels(), ["first", "after", "swarm"])

    def test_swarm_last(self):
        self.dispatch("sl", "--cwd", self.repo)
        self.new_workspace("after", self.repo)
        self.ok("swarm-last")
        self.assertEqual(self.ws_labels(), ["after", "swarm"])
        n = len(self.moves)
        self.ok("swarm-last")
        self.assertEqual(len(self.moves), n, "already last: a quiet no-op")
        self.ok("done", "sl")
        self.new_workspace("after3", self.repo)
        self.ok("swarm-last")
        self.assertEqual(self.ws_labels()[-1], "after3", "with no swarm, nothing is moved")

    def test_swarm_tab(self):
        out = self.ok("swarm-tab", "watch", "--cwd", self.repo).strip()
        p = self.pane(out)
        self.assertEqual(p["cwd"], str(self.repo))
        self.assertEqual(self.tab_labels(self.swarm()["workspace_id"]), ["watch"])
        self.assertEqual(self.agent_names(), [])
        self.assertEqual(self.records(), [], "no agent, no record")
        self.assertEqual(self.pane(self.ok("swarm-tab", "two").strip())["cwd"], str(self.home))
        self.refused("usage: swarm-tab", "swarm-tab", "a", "b")


class SecondWorktrees(Bed):
    def setUp(self):
        super().setUp()
        self.repo2, self.other2 = self.make_repo("repo2")
        (self.wt / "repo2").mkdir(parents=True)

    def linked(self, name, branch):
        path = self.wt / "repo2" / name
        sh("git", "-C", self.repo2, "worktree", "add", "-q", "-b", branch, str(path), "origin/main")
        return path

    def test_also_worktree_is_the_agents(self):
        w2 = self.linked("feat-two", "feat/two")
        self.dispatch("two", "--worktree", "two", "--cwd", self.repo, "--also-worktree", w2)
        self.assertIn(str(w2), json.dumps(self.record("two")))
        self.assertNotIn(str(w2), self.ok("orphans"), "not paneless while its agent lives")
        out = self.refused("belongs to live agent two", "done", w2)
        self.assertTrue(w2.is_dir(), "worktree untouched")
        self.ok("done", "two")
        self.assertFalse((self.wt / "repo" / "two").exists(), "its own worktree removed")
        self.assertFalse(w2.exists(), "and the second one")
        self.assertNotIn("feat/two", self.branches(self.repo2), "second branch deleted (merged)")

    def test_adopt(self):
        self.dispatch("three", "--cwd", self.repo)
        w3 = self.linked("feat-three", "feat/three")
        self.assertIn(str(w3), self.ok("orphans"), "before adopt it looks orphaned")
        self.assertIn("adopted", self.ok("adopt", "three", w3))
        self.assertNotIn(str(w3), self.ok("orphans"), "after adopt it is in use")
        self.assertIn("already has", self.ok("adopt", "three", w3))
        self.assertEqual(Path(self.records("three")[0]).read_text().count(str(w3)), 1, "adopt is idempotent")
        self.commit_in(w3, "e.txt")
        self.refused("not merged", "done", "three")
        self.assertIn("three", self.agent_names(), "agent untouched by the refusal")
        sh("git", "-C", w3, "reset", "-q", "--hard", "origin/main")
        self.ok("done", "three")
        self.assertFalse(w3.exists())

    def test_adopt_by_a_live_agent_without_a_record_and_errors(self):
        p = self.new_workspace("manual", self.repo)
        with self.mutate() as s:  # an agent started by hand, not dispatched
            s["agents"]["manual"] = {"name": "manual", "pane_id": p["pane_id"], "agent": "claude",
                                     "agent_status": "idle", "args": [], "prompts": [], "screen": "> "}
        w = self.linked("feat-man", "feat/man")
        self.ok("adopt", "manual", w)
        rec = self.record("manual")
        self.assertEqual(rec["placement"], "adopted")
        self.assertEqual(rec["sound"], "on")
        self.refused("no dispatch record and no live agent named ghost", "adopt", "ghost", w)
        self.refused("is not a linked git worktree", "adopt", "manual", self.repo)
        self.refused("usage: adopt", "adopt", "manual")

    def test_a_worktree_nobody_claims_is_an_orphan_and_done_path_removes_it(self):
        w4 = self.linked("feat-four", "feat/four")
        self.assertIn(f"{w4}  [feat/four, merged]  no pane is in it", self.ok("orphans"))
        self.ok("done", w4)
        self.assertFalse(w4.exists())

    def test_done_path_refuses_a_worktree_a_pane_is_in(self):
        w5 = self.linked("feat-five", "feat/five")
        p = self.new_workspace("in-it", str(w5))
        out = self.refused("still in use by", "done", w5)
        self.assertIn(p["pane_id"], out)
        self.refused("is not a linked git worktree", "done", self.repo)

    def test_a_dead_records_worktree_is_struck_from_it_by_done_path(self):
        self.dispatch("gone", "--worktree", "gone", "--cwd", self.repo)
        self.ok("done", "gone", "--keep-worktree")
        # keep the record, as a done interrupted after closing would
        rec = {"name": "gone", "placement": "swarm", "terminal_id": "term_dead", "worktree": str(self.wt / "repo" / "gone"),
               "branch": "gone", "repo": str(self.repo), "worktrees": []}
        (self.state / f"gone.{self.records_tag()}.json").write_text(json.dumps(rec))
        self.assertIn("(dispatched to gone, now gone", self.ok("orphans"))
        self.ok("done", self.wt / "repo" / "gone")
        self.assertNotIn("worktree", self.record("gone"))

    def records_tag(self):
        import hashlib
        return hashlib.sha1(self.env["HERDR_SOCKET_PATH"].encode()).hexdigest()[:8]


class Records(Bed):
    def tag(self):
        import hashlib
        return hashlib.sha1(self.env["HERDR_SOCKET_PATH"].encode()).hexdigest()[:8]

    def test_a_record_with_nothing_behind_it_is_an_orphan(self):
        self.state.mkdir()
        (self.state / f"ghost.{self.tag()}.json").write_text(json.dumps(
            {"name": "ghost", "placement": "split", "terminal_id": "term_x", "worktrees": []}))
        self.assertIn("ghost: dispatch record with nothing left behind it", self.ok("orphans"))
        self.ok("done", "ghost")
        self.assertEqual(self.records(), [])

    def test_records_from_before_owns_are_read_by_their_placement(self):
        a = self.dispatch("old", "--cwd", self.repo)
        extra = self.split_off(a["pane_id"])
        f = self.records("old")[0]
        rec = json.loads(f.read_text())
        del rec["owns"]
        f.write_text(json.dumps(rec))
        self.assertIn("with 2 pane(s)", self.ok("done", "old"))
        self.assertIsNone(self.pane(extra["pane_id"]))

    def test_a_python_era_record_is_read_as_is(self):
        # The original tool wrote this shape, and branch null for a detached
        # checkout; bowery files records under the same names.
        a = self.dispatch("py", "--cwd", self.repo)
        f = self.records("py")[0]
        f.write_text(json.dumps({
            "name": "py", "socket": self.env["HERDR_SOCKET_PATH"], "placement": "swarm", "kind": "claude",
            "started": "2026-10-30T12:00:00+00:00", "pane_id": a["pane_id"], "terminal_id": self.pane(a["pane_id"])["terminal_id"],
            "workspace_id": a["workspace_id"], "tab_id": a["tab_id"], "created_pane": True, "sound": "off",
            "worktrees": [{"path": "/nowhere", "repo": "/nowhere-repo", "branch": None}],
            "owns": {"kind": "tab", "id": a["tab_id"]}}, indent=2) + "\n")
        out = self.ok("done", "py")
        self.assertIn("worktree /nowhere already gone", out)
        self.assertEqual(self.agent_names(), [])

    def test_list_prints_an_orphan_count(self):
        self.dispatch("idle1", "--cwd", self.repo)
        rc, out = self.bowery("list")
        self.assertEqual(rc, 0)
        self.assertIn("1 orphan(s): bowery orphans", out)
        self.ok("done", "idle1")
        self.assertIn("no orphans", self.ok("orphans"))

    def test_names_for_completion(self):
        self.dispatch("cmp-a", "--cwd", self.repo)
        self.assertEqual(self.ok("names").split(), ["cmp-a"])


class Hazard(Bed):
    """A herdr-made worktree group: a sibling must survive every done.

    `herdr worktree create` groups worktree workspaces under a primary on the
    main checkout, and `pane close` on the primary's last pane closes the
    whole group (measured on herdr 0.9.5; the fake does the same).
    """

    def mk(self, name):
        return self.herdr("worktree", "create", "--cwd", self.repo, "--branch", name, "--base", "origin/main",
                          "--path", self.wt / "repo" / name, "--label", name, "--no-focus")["root_pane"]["pane_id"]

    def test_a_sibling_survives(self):
        h1, h2 = self.mk("h1"), self.mk("h2")
        prim = next(w for w in self.state_of()["workspaces"] if w.get("worktree") and not w["worktree"]["is_linked_worktree"])
        self.assertEqual(prim["label"], "repo", "herdr spawned a primary on the main checkout")
        prim_pane = next(p["pane_id"] for p in self.state_of()["panes"] if p["workspace_id"] == prim["workspace_id"])
        self.run_in(h2, "sleep 300")
        self.assertIn("leave it", self.ok("orphans"), "orphans flags the primary as leave-it")
        out = self.ok("done", h1)
        self.assertIn("left repo-root workspace", out, "primary left open while h2 is open")
        self.assertIsNotNone(self.pane(h2), "h2 survived")
        self.refused("would close them too", "done", prim_pane)
        self.assertIsNotNone(self.pane(h2), "h2 survived that too")
        out = self.ok("done", h2, "--force")
        self.assertIn("closed blank repo-root workspace", out)
        self.assertNotIn("repo", self.ws_labels())
        self.assertEqual(self.ws_labels(), [])


class LocalIdentity(Bed):
    """A shared box dispatching to its own server, as a person there."""

    def setUp(self):
        super().setUp()
        (self.home / "identities").mkdir()
        (self.home / "identities" / "alice.env").write_text("GIT_AUTHOR_NAME=alice\nGIT_COMMITTER_NAME=alice\n")
        self.config.write_text(f'identity_command = "{FAKES}/as"\n')

    def test_dispatch_as_a_local_identity(self):
        out = self.dispatch("mine", "--as", "alice", "--worktree", "mine", "--cwd", self.repo, "--", "go")
        self.assertEqual(out["as"], "alice")
        self.assertNotIn("machine", out)
        a = self.agent("mine")
        self.assertEqual(a["env"].get("BOWERY_AS"), "alice", "the pane's shell became alice before the agent started")
        self.assertEqual(self.record("mine")["as"], "alice")
        self.assertIn("alice\tgit -C", (self.home / "as.log").read_text(), "git ran as alice")

    def test_done_runs_git_as_the_identity(self):
        self.dispatch("mine", "--as", "alice", "--worktree", "mine", "--cwd", self.repo)
        (self.home / "as.log").write_text("")
        self.ok("done", "mine", "--force")
        self.assertIn("alice\tgit -C", (self.home / "as.log").read_text())

    def test_an_unknown_local_identity_is_refused(self):
        r = self.refused("no usable identity", "dispatch", "x", "--as", "bob", "--cwd", self.repo)
        self.assertIn("alice", r)

    def test_as_needs_a_local_identity_command(self):
        self.config.write_text("")
        self.refused("no identity_command", "dispatch", "x", "--as", "alice", "--cwd", self.repo)


if __name__ == "__main__":
    unittest.main()
