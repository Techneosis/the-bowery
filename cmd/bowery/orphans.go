package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type orphan struct{ kind, line, target string }

// checkoutsUnder lists checkout dirs under the worktrees root, without
// walking into them.
func checkoutsUnder(root string) ([]string, error) {
	if machine != "" { // <root>/<repo>/<branch>/.git, as dispatch makes them
		// Plain -print: -printf is GNU-only, and the box may be a Mac.
		rc, out, err := sshSh("find " + shq(root) + " -mindepth 2 -maxdepth 3 -name .git -print")
		if err != nil || rc != 0 {
			return nil, err
		}
		var found []string
		for _, l := range strings.Split(out, "\n") {
			if strings.TrimSpace(l) != "" {
				found = append(found, strings.TrimSuffix(l, "/.git"))
			}
		}
		sort.Strings(found)
		return found, nil
	}
	var found []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			found = append(found, p)
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(found)
	return found, nil
}

func findOrphans() ([]orphan, error) {
	ags, err := agents()
	if err != nil {
		return nil, err
	}
	ps, err := panes()
	if err != nil {
		return nil, err
	}
	all, err := workspaces()
	if err != nil {
		return nil, err
	}
	recs := map[string]*Record{}
	var names []string
	for _, r := range allRecords() {
		recs[r.Name] = r
		names = append(names, r.Name)
	}
	var out []orphan
	for _, a := range ags {
		// A shared box also runs other people's dispatches, quiet-tagged too:
		// there only a record made here says the agent is ours.
		_, ours := recs[a.Name]
		dispatched := (a.Name != "" && ours) || (machine == "" && a.Tokens["sound"] == "off")
		if dispatched && (a.AgentStatus == "idle" || a.AgentStatus == "done" || a.AgentStatus == "blocked") {
			id := a.Name
			if id == "" {
				id = a.PaneID
			}
			line := fmt.Sprintf("%s  %s  %s  %s", id, a.AgentStatus, a.PaneID, a.Cwd)
			if r := recs[a.Name]; r != nil && r.Started != "" {
				line += fmt.Sprintf("  (dispatched %s)", r.Started)
			}
			out = append(out, orphan{"agent", line, id})
		}
	}
	claims := worktreeClaims(ags, ps)
	root, err := worktreeRoot()
	if err != nil {
		return nil, err
	}
	if d, err := isDir(root); err != nil {
		return nil, err
	} else if d {
		found, err := checkoutsUnder(root)
		if err != nil {
			return nil, err
		}
		for _, path := range found {
			info, err := worktreeInfo(path)
			if err != nil {
				return nil, err
			}
			if info == nil || len(occupants(path, ps, nil)) > 0 {
				continue
			}
			owners := claims[norm(path)]
			live := false
			for _, c := range owners {
				live = live || c.live
			}
			if live {
				continue // a live agent's second worktree: in use, pane or no pane
			}
			ref, err := defaultBranch(info.repo)
			if err != nil {
				return nil, err
			}
			rev := info.branch
			if rev == "" {
				rev = "HEAD"
			}
			state := "unmerged"
			if d, err := dirty(path, true); err != nil {
				return nil, err
			} else if d {
				state = "dirty"
			} else if ref != "" {
				if m, err := mergedInto(path, rev, ref); err != nil {
					return nil, err
				} else if m != "" {
					state = "merged"
				}
			}
			whose := ""
			switch {
			case len(owners) > 0:
				whose = fmt.Sprintf("  (dispatched to %s, now gone)", owners[0].name)
			case machine != "":
				whose = "  (no record here: maybe another dispatcher's)"
			}
			b := info.branch
			if b == "" {
				b = "detached"
			}
			out = append(out, orphan{"worktree", fmt.Sprintf("%s  [%s, %s]  no pane is in it%s", path, b, state, whose), path})
		}
	}
	for _, ws := range all {
		if !isPrimary(ws) {
			continue
		}
		why := strayPrimaryVerdict(ws, all, ps)
		if why != "" && !strings.Contains(why, siblingsNote) {
			continue // not a blank herdr-spawned one: someone's real workspace
		}
		note := ""
		if why != "" {
			note = " — leave it: " + why
		}
		out = append(out, orphan{"workspace", fmt.Sprintf("%s (%s) blank repo-root workspace%s", ws.WorkspaceID, ws.Label, note),
			ws.Worktree.CheckoutPath})
	}
	live := map[string]bool{}
	for _, a := range ags {
		live[a.Name] = true
	}
	terms := map[string]bool{}
	for _, p := range ps {
		terms[p.TerminalID] = true
	}
	for _, name := range names {
		r := recs[name]
		if live[name] || (r.TerminalID != "" && terms[r.TerminalID]) {
			continue
		}
		if c := ownedContainer(r, ps, all); c != nil {
			out = append(out, orphan{"record", fmt.Sprintf("%s: agent gone, its %s %s still open with %d pane(s)", name, c.kind, c.id, len(c.panes)), name})
			continue
		}
		left := false
		for _, w := range recWorktrees(r) {
			ex, err := exists(w.Path)
			if err != nil {
				return nil, err
			}
			left = left || ex
		}
		if !left {
			out = append(out, orphan{"record", name + ": dispatch record with nothing left behind it", name})
		}
	}
	return out, nil
}

// machinesToScan is the local server ("") and every machine with a dispatch
// record here; --machine narrows it to that one.
func machinesToScan(explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	return append([]string{""}, recordedMachines()...)
}

// onMachine runs fn against machine m. A box that can't be reached is
// reported, not fatal, so it never hides the local results.
func onMachine[T any](m string, fn func() (T, error)) (T, bool) {
	useMachine(m, "")
	defer useMachine("", "")
	v, err := fn()
	if err != nil {
		if m == "" {
			die("%v", herdrMsg(err))
		}
		warn("%s: unreachable: %v", m, herdrMsg(err))
		var zero T
		return zero, false
	}
	return v, true
}

func cmdOrphans(argv []string) {
	explicit, argv := popMachine(argv)
	if len(argv) > 0 {
		die("orphans: unexpected %s", strings.Join(argv, " "))
	}
	type perMachine struct {
		m     string
		found []orphan
	}
	var per []perMachine
	any := false
	for _, m := range machinesToScan(explicit) {
		found, _ := onMachine(m, findOrphans)
		per = append(per, perMachine{m, found})
		any = any || len(found) > 0
	}
	if !any {
		say("no orphans")
		return
	}
	for _, p := range per {
		if len(p.found) == 0 {
			continue
		}
		ind := ""
		if p.m != "" {
			say("on machine %s (done finds a name by its record; a path there needs --machine %s):", p.m, p.m)
			ind = "  "
		}
		printOrphans(p.found, ind)
	}
}

func printOrphans(found []orphan, ind string) {
	titles := []struct{ kind, title string }{
		{"agent", "finished dispatched agents still alive (done <name>)"},
		{"worktree", "worktrees no pane is in (done <path>)"},
		{"workspace", "blank repo-root workspaces (closed by the last done of that repo)"},
		{"record", "dispatch records whose agent is gone (done <name> clears them)"},
	}
	for _, t := range titles {
		first := true
		for _, o := range found {
			if o.kind != t.kind {
				continue
			}
			if first {
				say("%s%s:", ind, t.title)
				first = false
			}
			say("%s  %s", ind, o.line)
		}
	}
}

func cmdList(argv []string) {
	explicit, argv := popMachine(argv)
	if len(argv) > 0 {
		die("list: unexpected %s", strings.Join(argv, " "))
	}
	n := 0
	for _, m := range machinesToScan(explicit) {
		if m != "" {
			say("== machine %s", m)
		}
		c, _ := onMachine(m, func() (int, error) {
			recs := map[string]*Record{}
			for _, r := range allRecords() {
				recs[r.Name] = r
			}
			ags, err := agents()
			if err != nil {
				return 0, err
			}
			for _, a := range ags {
				snd := a.Tokens["sound"]
				if snd == "" {
					snd = "on"
				}
				extra := a.Title
				if r := recs[a.Name]; r != nil && r.As != "" {
					extra = "[as " + r.As + "] " + extra
				}
				nm := a.Name
				if nm == "" {
					nm = "-"
				}
				say("%-8s %-4s %-8s %-16s %s", a.PaneID, snd, a.AgentStatus, nm, extra)
			}
			found, err := findOrphans()
			if err != nil {
				return 0, nil
			}
			return len(found), nil
		})
		n += c
	}
	if n > 0 {
		warn("%d orphan(s): bowery orphans", n)
	}
}

func cmdMute(cmd string, argv []string) {
	explicit, rest := popMachine(argv)
	if len(rest) == 0 {
		die("%s: which agent?", cmd)
	}
	for _, t := range rest {
		if err := pickMachine(t, explicit); err != nil {
			die("%v", herdrMsg(err))
		}
		p, err := resolvePane(t)
		if err != nil {
			die("%v", herdrMsg(err))
		}
		if p == "" {
			die("no agent or pane named %s", t)
		}
		mode := "on"
		if cmd == "mute" {
			mode = "off"
		}
		if err := setSound(p, mode); err != nil {
			die("%v", herdrMsg(err))
		}
		say("%s (%s): %s", t, p, cmd)
	}
}

// cmdNames prints recorded agent names, for tab completion.
func cmdNames() {
	files, _ := filepath.Glob(filepath.Join(stateDir(), "*.json"))
	seen := map[string]bool{}
	for _, f := range files {
		if r := readRecord(f); r != nil && !seen[r.Name] {
			seen[r.Name] = true
			fmt.Println(r.Name)
		}
	}
}

func herdrMsg(err error) string {
	var he *HerdrError
	if errors.As(err, &he) {
		return he.Raw
	}
	return err.Error()
}
