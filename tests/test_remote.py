"""--machine and --as: dispatch on a saved herdr machine, through the fakes.

The box is a directory here reached through tests/fakes/ssh (which runs the
remote command locally with HOME set to the box), and its herdr server is
the fake's machine-swarm-box.json. The identity command is tests/fakes/as,
with one identity, alice. Nothing leaves this machine.

    python3 -m unittest discover tests
"""

import hashlib
import json
import unittest

from _fake import BOX, FAKES, Bed, sh


def tag(key):
    return hashlib.sha1(key.encode()).hexdigest()[:8]


class ArgumentChecks(Bed):
    """Refused before anything is made, anywhere."""

    def test_refusals(self):
        self.add_box()
        cases = [
            ("--as picks an identity, and this machine has no identity_command", ["--as", "alice"]),
            (f"--machine {BOX} needs --as", ["--machine", BOX]),
            ("--as must be lowercase", ["--machine", BOX, "--as", "Bad Name"]),
            ("--split, --tab and --from", ["--machine", BOX, "--as", "t", "--split"]),
            ("--split, --tab and --from", ["--machine", BOX, "--as", "t", "--tab"]),
            ("--split, --tab and --from", ["--machine", BOX, "--as", "t", "--from", "w1:p1"]),
            ("--worktree with --machine needs --cwd", ["--machine", BOX, "--as", "t", "--worktree", "b"]),
            ("--machine needs a value", ["--as", "t", "--machine"]),
            ("--as needs a value", ["--machine", BOX, "--as"]),
            ("no saved herdr machine labelled no-such-box", ["--machine", "no-such-box"]),
            ("no usable identity on swarm-box", ["--machine", BOX, "--as", "bob"]),
        ]
        for want, args in cases:
            with self.subTest(args=args):
                self.refused(want, "dispatch", "argcheck", *args)
        self.assertEqual(self.ws_labels(BOX), [], "none of that made a workspace")
        self.assertEqual(self.records(), [], "nor a record")

    def test_an_unknown_identity_names_the_ones_there(self):
        self.add_box()
        out = self.refused("identities there: alice", "dispatch", "x", "--machine", BOX, "--as", "bob")
        self.assertIn("no identity 'bob'", out)

    def test_as_is_refused_for_a_machine_without_identities(self):
        self.add_box(identity=False)
        self.refused("has no identity_command", "dispatch", "x", "--machine", BOX, "--as", "alice")

    def test_a_missing_repo_on_the_box_is_named(self):
        self.add_box()
        self.refused("does not exist on swarm-box — clone the repo there first",
                     "dispatch", "x", "--machine", BOX, "--as", "alice", "--worktree", "b", "--cwd", "~/dev/missing")
        self.refused("not one relative to here", "dispatch", "x", "--machine", BOX, "--as", "alice", "--cwd", "dev/app")

    def test_an_unreachable_box_is_named(self):
        (self.herdr_dir / "machines.json").write_text(json.dumps([{"label": "dark", "target": "dark", "enabled": True}]))
        self.refused("ssh dark failed", "dispatch", "x", "--machine", "dark")

    def test_a_disabled_profile_is_refused(self):
        (self.herdr_dir / "machines.json").write_text(json.dumps([{"label": "off", "target": "off", "enabled": False}]))
        self.refused("is disabled", "dispatch", "x", "--machine", "off")


class Unreachable(Bed):
    """A remote record is not a local orphan, and an unreachable machine
    doesn't break orphans or list."""

    def test_records_of_a_machine_that_cannot_be_reached(self):
        self.state.mkdir()
        f = self.state / f"rmt.{tag('machine:no-such-box')}.json"
        f.write_text(json.dumps({"name": "rmt", "machine": "no-such-box", "as": "t", "placement": "swarm",
                                 "terminal_id": "term_x", "worktrees": []}))
        rc, out = self.bowery("orphans")
        self.assertEqual(rc, 0, out)
        self.assertNotIn("rmt: dispatch record", out, "not listed as a local stale record")
        self.assertIn("no-such-box: unreachable", out)
        rc, out = self.bowery("list")
        self.assertEqual(rc, 0, out)
        self.assertIn("== machine no-such-box", out)
        rc, out = self.bowery("done", "rmt")
        self.assertEqual(rc, 1, "done of a remote name goes to its machine, and fails there, not locally")
        self.assertIn("no-such-box", out)
        self.assertTrue(f.exists(), "the record survives a done that couldn't reach its machine")


class Remote(Bed):
    def setUp(self):
        super().setUp()
        self.add_box()
        self.app, self.app_other = self.box_repo()

    def test_dispatch_on_the_box_as_an_identity(self):
        out = self.dispatch("far", "--machine", BOX, "--as", "alice", "--worktree", "far", "--cwd", "~/dev/app",
                            "--", "go")
        wt = self.box / ".herdr" / "worktrees" / "app" / "far"
        self.assertEqual(out["worktree"], str(wt), "the box's herdr worktrees dir, ~ being the box's home")
        self.assertEqual((out["machine"], out["as"]), (BOX, "alice"))
        self.assertTrue(wt.is_dir())
        self.assertEqual(self.ws_labels(), [], "nothing on the local server")
        sw = self.swarm(BOX)
        self.assertEqual(sw["cwd"], str(self.box), "swarm on the box, rooted at its home")
        a = self.agent("far", BOX)
        self.assertEqual(a["env"].get("BOWERY_AS"), "alice", "the agent's shell became alice before it started")
        self.assertEqual(a["env"].get("GIT_AUTHOR_NAME"), "alice")
        self.assertEqual(a["prompts"], [{"text": "go", "wait": False}])
        self.assertEqual(self.pane(a["pane_id"], BOX)["tokens"], {"sound": "off"})
        rec = self.record("far")
        self.assertEqual((rec["machine"], rec["host"], rec["as"], rec["socket"]), (BOX, BOX, "alice", None))
        self.assertEqual(self.records("far")[0].name, f"far.{tag('machine:' + BOX)}.json")
        as_log = (self.box / "as.log").read_text()
        self.assertIn("alice\tgit -C", as_log, "remote git ran as alice")
        self.assertIn("worktree add", as_log)

    def test_ssh_gives_up_on_a_dead_box(self):
        # A box that has gone away mid-connection must fail ssh, not hang a
        # job that runs bowery blind: a connect timeout, and keepalives.
        self.dispatch("far", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app", "--", "go")
        calls = (self.herdr_dir / "ssh-opts.log").read_text().splitlines()
        self.assertTrue(calls)
        for opts in calls:
            o = opts.split()
            self.assertIn("BatchMode=yes", o)
            self.assertIn("ConnectTimeout=10", o)
            self.assertIn("ServerAliveInterval=15", o)
            self.assertIn("ServerAliveCountMax=4", o)

    def test_a_home_path_maps_to_the_box(self):
        out = self.dispatch("mapped", "--machine", BOX, "--as", "alice", "--cwd", f"{self.home}/dev/app")
        self.assertEqual(self.pane(out["pane_id"], BOX)["cwd"], str(self.box / "dev" / "app"))

    def test_trust_on_the_box_follows_its_roots(self):
        (self.box / "elsewhere").mkdir()
        self.refused("outside", "dispatch", "x", "--machine", BOX, "--as", "alice", "--cwd", "~/elsewhere")
        self.assertEqual(self.tab_labels(self.swarm(BOX)["workspace_id"], BOX) if self.swarm(BOX) else [], [])
        self.add_box(extra='trust_roots = ["~/elsewhere"]\n')
        self.dispatch("y", "--machine", BOX, "--as", "alice", "--cwd", "~/elsewhere")

    def test_done_finds_a_remote_agent_by_its_record(self):
        self.dispatch("far", "--machine", BOX, "--as", "alice", "--worktree", "far", "--cwd", "~/dev/app")
        wt = self.box / ".herdr" / "worktrees" / "app" / "far"
        self.commit_in(wt, "f.txt")
        self.refused("not merged into origin/main", "done", "far")
        self.merge_pr("far", other=self.app_other)
        open(self.box / "as.log", "w").close()
        out = self.ok("done", "far")
        self.assertIn("closed tab", out)
        self.assertFalse(wt.exists())
        self.assertNotIn("far", self.branches(self.app))
        self.assertIn("fast-forwarded", out)
        self.assertIsNone(self.agent("far", BOX))
        self.assertEqual(self.records(), [])
        self.assertIn("alice\tgit -C", (self.box / "as.log").read_text(), "done's git ran as alice too")

    def test_a_local_name_wins_and_machine_picks(self):
        self.dispatch("twin", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app")
        self.dispatch("twin", "--cwd", self.repo)
        self.ok("done", "twin")
        self.assertIsNone(self.agent("twin"))
        self.assertIsNotNone(self.agent("twin", BOX), "a local name wins a clash")
        self.ok("done", "twin", "--machine", BOX)
        self.assertIsNone(self.agent("twin", BOX))

    def test_mute_and_list_on_the_box(self):
        out = self.dispatch("far", "--machine", BOX, "--as", "alice", "--loud", "--cwd", "~/dev/app")
        self.ok("mute", "far")
        self.assertEqual(self.pane(out["pane_id"], BOX)["tokens"], {"sound": "off"})
        listed = self.ok("list")
        self.assertIn(f"== machine {BOX}", listed)
        self.assertIn("[as alice]", listed)
        self.assertIn("far", self.ok("list", "--machine", BOX))

    def test_orphans_on_a_shared_box_count_only_ours(self):
        self.dispatch("mine", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app")
        # Someone else's quiet agent, dispatched from another machine.
        theirs = self.herdr("workspace", "create", "--label", "theirs", "--cwd", str(self.box), machine=BOX)["root_pane"]
        with self.mutate(BOX) as s:
            s["agents"]["theirs"] = {"name": "theirs", "pane_id": theirs["pane_id"], "agent": "claude",
                                     "agent_status": "idle", "args": [], "prompts": [], "screen": "> "}
            next(p for p in s["panes"] if p["pane_id"] == theirs["pane_id"])["tokens"] = {"sound": "off"}
        # ...and a worktree no record here claims.
        loose = self.box / ".herdr" / "worktrees" / "app" / "loose"
        sh("git", "-C", self.app, "worktree", "add", "-q", "-b", "loose", str(loose), "origin/main")
        out = self.ok("orphans")
        self.assertIn(f"on machine {BOX}", out)
        self.assertIn("mine  idle", out)
        self.assertNotIn("theirs", out)
        self.assertIn("(no record here: maybe another dispatcher's)", out)
        self.assertIn("mine", self.ok("orphans", "--machine", BOX))

    def test_orphans_check_a_gone_agent_as_its_identity(self):
        # resume looks at the agent's directory as its identity; orphans and
        # list must ask the same question, or they offer a resume that fails.
        far = self.dispatch("far", "--machine", BOX, "--as", "alice", "--worktree", "far", "--cwd", "~/dev/app")
        self.herdr("tab", "close", far["tab_id"], machine=BOX)
        (self.box / "as.log").write_text("")
        out = self.ok("orphans")
        self.assertIn("bowery resume far", out)
        self.assertIn(f"alice\tsh -c if test -d {far['worktree']}", (self.box / "as.log").read_text())

    def test_orphans_dont_offer_a_resume_that_would_start_as_the_wrong_person(self):
        far = self.dispatch("who", "--machine", BOX, "--as", "alice", "--worktree", "who", "--cwd", "~/dev/app")
        self.herdr("tab", "close", far["tab_id"], machine=BOX)
        self.assertIn("bowery resume who", self.ok("orphans"))
        conf = self.config.read_text()
        self.config.write_text("\n".join(l for l in conf.splitlines() if not l.startswith("identity_command")) + "\n")
        out = self.ok("orphans")
        self.assertNotIn("bowery resume who", out, "resume would refuse it")
        self.assertIn("who: agent gone; resume would refuse it", out, "said, not dropped from the list")
        self.refused("it would start as the wrong person", "resume", "who")

    def test_a_record_blocked_only_by_identity_is_not_nothing_left(self):
        # No worktree, its folder still there: only the config changed. It
        # must not read as junk for done to clear (done discards the session).
        far = self.dispatch("who2", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app")
        self.herdr("tab", "close", far["tab_id"], machine=BOX)
        conf = self.config.read_text()
        self.config.write_text("\n".join(l for l in conf.splitlines() if not l.startswith("identity_command")) + "\n")
        out = self.ok("orphans")
        self.assertIn("who2: agent gone; resume would refuse it", out)
        self.assertNotIn("who2: dispatch record with nothing left", out)

    def test_a_failing_find_on_the_box_is_not_no_worktrees(self):
        self.dispatch("far", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app")
        root = self.box / ".herdr" / "worktrees"
        root.mkdir(parents=True, exist_ok=True)
        root.chmod(0)
        try:
            out = self.ok("orphans")
        finally:
            root.chmod(0o755)
        self.assertIn(f"{BOX}", out)
        self.assertIn("find under", out)

    def test_one_records_ssh_failure_is_that_records(self):
        a = self.dispatch("aa", "--machine", BOX, "--as", "alice", "--worktree", "aa", "--cwd", "~/dev/app")
        b = self.dispatch("bb", "--machine", BOX, "--as", "alice", "--worktree", "bb", "--cwd", "~/dev/app")
        for x in (a, b):
            self.herdr("tab", "close", x["tab_id"], machine=BOX)
        # resumable's check runs as alice; orphans' own look at the worktree
        # doesn't, and that is the call that drops.
        out = self.ok("orphans", env={"FAKE_SSH_FAIL_ON": f"test -e {a['worktree']}"})
        self.assertNotIn("unreachable", out)
        self.assertIn("aa: can't tell whether its worktree", out)
        self.assertIn("bb: agent gone, its worktree still there", out)
        self.assertNotIn("aa: dispatch record with nothing left", out)

    def test_a_worktree_path_on_the_box_needs_machine(self):
        loose = self.box / ".herdr" / "worktrees" / "app" / "loose"
        loose.parent.mkdir(parents=True)
        sh("git", "-C", self.app, "worktree", "add", "-q", "-b", "loose", str(loose), "origin/main")
        self.ok("done", "~/.herdr/worktrees/app/loose", "--machine", BOX)
        self.assertFalse(loose.exists())

    def test_adopt_on_the_box(self):
        self.dispatch("far", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app")
        extra = self.box / ".herdr" / "worktrees" / "app" / "extra"
        extra.parent.mkdir(parents=True)
        sh("git", "-C", self.app, "worktree", "add", "-q", "-b", "extra", str(extra), "origin/main")
        self.ok("adopt", "far", "~/.herdr/worktrees/app/extra")
        self.assertEqual(self.record("far")["worktrees"][0]["path"], str(extra))
        self.ok("done", "far")
        self.assertFalse(extra.exists())

    def test_swarm_tab_on_the_box(self):
        pid = self.ok("swarm-tab", "watch", "--machine", BOX, "--cwd", "~/dev/app").strip()
        self.assertEqual(self.pane(pid, BOX)["cwd"], str(self.box / "dev" / "app"))

    def test_a_machine_without_identities_runs_as_the_ssh_user(self):
        self.add_box(identity=False)
        self.dispatch("plain", "--machine", BOX, "--cwd", "~/dev/app")
        self.assertNotIn("BOWERY_AS", self.agent("plain", BOX)["env"])
        self.assertNotIn("as", self.record("plain"))

    def test_the_ssh_destination_can_be_configured(self):
        self.add_box(extra=f'ssh = "{BOX}"\n')
        (self.herdr_dir / "machines.json").write_text(json.dumps([{"label": BOX, "target": "not-resolvable"}]))
        self.dispatch("viacfg", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app")

    def test_a_custom_identity_variable(self):
        wrapper = self.t / "become"
        wrapper.write_text(f'#!/usr/bin/env bash\nw=$1; shift; export WHO_AM_I=$w; exec {FAKES}/as "$w" "$@"\n')
        wrapper.chmod(0o755)
        self.config.write_text(f'[machines.{BOX}]\nidentity_command = "{wrapper}"\nidentity_env = "WHO_AM_I"\n')
        self.dispatch("cust", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app")
        self.assertEqual(self.agent("cust", BOX)["env"]["WHO_AM_I"], "alice")


if __name__ == "__main__":
    unittest.main()
