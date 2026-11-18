"""--ticket on dispatch, kept by resume; list --json, here and on a box.

    python3 -m unittest discover tests
"""

import hashlib
import json

from _fake import BOX, Bed

def tag(key):
    return hashlib.sha1(key.encode()).hexdigest()[:8]


KEYS = {"name", "machine", "as", "state", "title", "ticket", "worktree", "branch", "dispatched_at", "last_seen",
        "pane", "sound", "dispatched", "resumable", "resumes", "note"}


class Ticket(Bed):
    def listed(self, *args):
        out = self.ok("list", "--json", *args)
        js, _ = json.JSONDecoder().raw_decode(out)
        return js

    def by_name(self, js):
        return {e["name"]: e for e in js}

    def test_dispatch_records_the_ticket_and_prints_it(self):
        out = self.dispatch("tk", "--ticket", "DEV-551", "--cwd", self.repo)
        self.assertEqual(out["ticket"], "DEV-551")
        self.assertEqual(self.record("tk")["ticket"], "DEV-551")

    def test_no_ticket_leaves_the_record_and_output_as_they_were(self):
        out = self.dispatch("plain", "--cwd", self.repo)
        self.assertNotIn("ticket", out)
        self.assertNotIn("ticket", self.record("plain"))

    def test_a_bad_ticket_is_refused_before_anything_is_made(self):
        for bad in ("-x", "a b", "a/b", "_lead", "x" * 81, "dev;rm"):
            self.refused("--ticket", "dispatch", "bad", "--ticket", bad, "--cwd", self.repo)
        self.refused("--ticket needs a ticket id", "dispatch", "bad", "--ticket")
        self.refused("given twice", "dispatch", "bad", "--ticket", "A-1", "--ticket", "A-2", "--cwd", self.repo)
        self.assertEqual(self.agent_names(), [])
        self.assertEqual(self.records(), [])
        self.dispatch("long", "--ticket", "x" * 80, "--cwd", self.repo)
        self.dispatch("same", "--ticket", "A-1", "--ticket", "A-1", "--cwd", self.repo)
        self.dispatch("item", "--ticket", "ch.item_42", "--cwd", self.repo)

    def test_resume_keeps_the_ticket(self):
        self.dispatch("back", "--ticket", "DEV-7", "--worktree", "back", "--cwd", self.repo)
        self.herdr("tab", "close", self.record("back")["tab_id"])
        self.ok("resume", "back")
        self.assertIsNotNone(self.agent("back"))
        rec = self.record("back")
        self.assertEqual((rec["ticket"], rec["resumes"]), ("DEV-7", 1))
        self.assertEqual(self.by_name(self.listed())["back"]["ticket"], "DEV-7")

    def test_list_json_live_and_gone(self):
        self.dispatch("live", "--ticket", "DEV-1", "--worktree", "live", "--cwd", self.repo)
        self.dispatch("bare", "--cwd", self.repo)
        self.dispatch("dead", "--ticket", "DEV-2", "--worktree", "dead", "--cwd", self.repo)
        self.set_status("bare", "working")
        self.herdr("tab", "close", self.record("dead")["tab_id"])
        js = self.listed()
        for e in js:
            self.assertEqual(set(e), KEYS)
        es = self.by_name(js)
        self.assertEqual(set(es), {"live", "bare", "dead"})
        live = es["live"]
        self.assertEqual((live["state"], live["ticket"], live["branch"], live["machine"], live["as"]),
                         ("idle", "DEV-1", "live", None, None))
        self.assertEqual(live["worktree"], str(self.wt / "repo" / "live"))
        self.assertEqual(live["dispatched_at"], self.record("live")["started"])
        self.assertTrue(live["last_seen"])
        self.assertTrue(live["dispatched"])
        self.assertEqual(live["pane"], self.record("live")["pane_id"])
        bare = es["bare"]
        self.assertEqual((bare["state"], bare["ticket"], bare["worktree"], bare["branch"]),
                         ("working", None, None, None))
        dead = es["dead"]
        self.assertEqual((dead["state"], dead["ticket"], dead["branch"], dead["last_seen"], dead["title"]),
                         ("gone", "DEV-2", "dead", None, None))
        self.assertTrue(dead["resumable"])
        self.assertEqual(dead["note"], "bowery resume dead")

    def test_list_json_with_nothing_is_an_empty_array(self):
        self.assertEqual(self.listed(), [])

    def test_list_json_shows_an_agent_bowery_did_not_dispatch(self):
        pane = self.new_workspace("hand", self.repo)["pane_id"]
        with self.mutate() as s:
            s["agents"]["byhand"] = {"name": "byhand", "pane_id": pane, "agent": "claude", "agent_status": "blocked",
                                     "args": [], "prompts": [], "screen": "> "}
        e = self.by_name(self.listed())["byhand"]
        self.assertEqual((e["state"], e["dispatched"], e["dispatched_at"], e["ticket"]), ("blocked", False, None, None))

    def test_list_text_is_unchanged_by_a_ticket(self):
        self.dispatch("t1", "--ticket", "DEV-9", "--cwd", self.repo)
        listed = self.ok("list")
        self.assertRegex(listed, r"\s+off\s+idle\s+t1\s")
        self.assertNotIn("DEV-9", listed)
        self.refused("list: unexpected --bogus", "list", "--bogus")
        self.refused("list: unexpected", "list", "--json", "extra")


class RemoteTicket(Ticket.__base__):
    def setUp(self):
        super().setUp()
        self.add_box()
        self.app, self.app_other = self.box_repo()

    def test_list_json_covers_the_box(self):
        self.dispatch("far", "--machine", BOX, "--as", "alice", "--ticket", "DEV-4", "--worktree", "far",
                      "--cwd", "~/dev/app")
        out = self.ok("list", "--json")
        js, _ = json.JSONDecoder().raw_decode(out)
        (far,) = [e for e in js if e["name"] == "far"]
        self.assertEqual((far["machine"], far["as"], far["ticket"], far["state"], far["branch"]),
                         (BOX, "alice", "DEV-4", "idle", "far"))
        self.assertEqual(far["worktree"], str(self.box / ".herdr" / "worktrees" / "app" / "far"))
        only = json.JSONDecoder().raw_decode(self.ok("list", "--json", "--machine", BOX))[0]
        self.assertEqual([e["name"] for e in only], ["far"])
        self.herdr("tab", "close", json.loads(self.records("far")[0].read_text())["tab_id"], machine=BOX)
        (gone,) = [e for e in json.JSONDecoder().raw_decode(self.ok("list", "--json"))[0] if e["name"] == "far"]
        self.assertEqual((gone["state"], gone["machine"], gone["ticket"]), ("gone", BOX, "DEV-4"))

    def test_resume_on_the_box_keeps_the_ticket(self):
        self.dispatch("far", "--machine", BOX, "--as", "alice", "--ticket", "DEV-5", "--worktree", "far",
                      "--cwd", "~/dev/app")
        self.herdr("tab", "close", json.loads(self.records("far")[0].read_text())["tab_id"], machine=BOX)
        self.ok("resume", "far")
        self.assertIsNotNone(self.agent("far", BOX))
        self.assertEqual(json.loads(self.records("far")[0].read_text())["ticket"], "DEV-5")

    def test_an_unreachable_box_is_an_entry_not_a_silence(self):
        self.dispatch("far", "--machine", BOX, "--as", "alice", "--cwd", "~/dev/app")
        dark = self.state / f"rmt.{tag('machine:no-such-box')}.json"
        dark.write_text(json.dumps({"name": "rmt", "machine": "no-such-box", "as": "t", "placement": "swarm",
                                    "terminal_id": "term_x", "worktrees": [], "ticket": "DEV-6"}))
        out = self.ok("list", "--json")
        js, _ = json.JSONDecoder().raw_decode(out)
        self.assertEqual(sorted((e["state"], e["machine"], e["name"]) for e in js),
                         [("idle", BOX, "far"), ("unreachable", "no-such-box", None)])
        self.assertIn("no-such-box: unreachable", out[out.index("]"):], "and a warning")
