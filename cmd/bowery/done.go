package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type doneOpts struct{ force, keepWorktree, keepBranch, dryRun bool }

// findTarget resolves a name or pane id to its record, live agent and pane.
func findTarget(target string) (*Record, *Agent, *Pane, error) {
	var rec *Record
	if !strings.Contains(target, "/") {
		rec = loadRecord(target)
	}
	ags, err := agents()
	if err != nil {
		return nil, nil, nil, err
	}
	ps, err := panes()
	if err != nil {
		return nil, nil, nil, err
	}
	var agent *Agent
	for i := range ags {
		if ags[i].Name == target || ags[i].PaneID == target {
			agent = &ags[i]
			break
		}
	}
	paneBy := func(match func(Pane) bool) *Pane {
		for i := range ps {
			if match(ps[i]) {
				return &ps[i]
			}
		}
		return nil
	}
	var pane *Pane
	switch {
	case agent != nil:
		pane = paneBy(func(p Pane) bool { return p.PaneID == agent.PaneID })
	case rec != nil:
		// The agent may have exited back to a shell, or the pane been moved:
		// terminal_id survives both, pane_id does not.
		if rec.TerminalID != "" {
			pane = paneBy(func(p Pane) bool { return p.TerminalID == rec.TerminalID })
		}
	case strings.Contains(target, ":"):
		pane = paneBy(func(p Pane) bool { return p.PaneID == target })
	}
	if pane != nil && agent == nil {
		for i := range ags {
			if ags[i].PaneID == pane.PaneID {
				agent = &ags[i]
				break
			}
		}
	}
	if rec == nil && agent != nil && agent.Name != "" {
		rec = loadRecord(agent.Name)
	}
	return rec, agent, pane, nil
}

func occupants(path string, ps []Pane, except map[string]bool) []Pane {
	var out []Pane
	for _, p := range ps {
		if except[p.PaneID] {
			continue
		}
		if (p.Cwd != "" && under(p.Cwd, path)) || (p.ForegroundCwd != "" && under(p.ForegroundCwd, path)) {
			out = append(out, p)
		}
	}
	return out
}

type container struct {
	kind, id, label string
	ws              Workspace
	panes           []Pane
}

// ownedContainer is the tab/workspace bowery made for rec's agent, if it is
// still open. Checked by label as well as id, so a tab someone renamed or an
// id herdr reused is never taken for it.
// An error is herdr failing to answer; a tab that is gone is (nil, nil), so a
// passing hiccup never reads as "its tab is closed".
func ownedContainer(rec *Record, ps []Pane, all []Workspace) (*container, error) {
	o := recOwns(rec)
	if o == nil {
		return nil, nil
	}
	wsByID := func(id string) *Workspace {
		for i := range all {
			if all[i].WorkspaceID == id {
				return &all[i]
			}
		}
		return nil
	}
	if o.Kind == "workspace" {
		ws := wsByID(o.ID)
		if ws == nil || ws.Label != rec.Name {
			return nil, nil
		}
		c := &container{kind: "workspace", id: ws.WorkspaceID, label: ws.Label, ws: *ws}
		for _, p := range ps {
			if p.WorkspaceID == ws.WorkspaceID {
				c.panes = append(c.panes, p)
			}
		}
		return c, nil
	}
	var r struct{ Tab Tab }
	if err := herdr(&r, "tab", "get", o.ID); err != nil {
		var he *HerdrError
		if errors.As(err, &he) && he.Code == "tab_not_found" {
			return nil, nil
		}
		return nil, fail("herdr tab get %s: %v", o.ID, err)
	}
	if r.Tab.Label != rec.Name {
		return nil, nil
	}
	ws := wsByID(r.Tab.WorkspaceID)
	if ws == nil {
		return nil, nil
	}
	c := &container{kind: "tab", id: r.Tab.TabID, label: r.Tab.Label, ws: *ws}
	for _, p := range ps {
		if p.TabID == r.Tab.TabID {
			c.panes = append(c.panes, p)
		}
	}
	return c, nil
}

func paneDesc(p Pane) string {
	what := ""
	if p.Agent != "" {
		what = strings.TrimSpace(p.Agent + " agent " + p.AgentStatus)
	}
	if what == "" {
		what = p.Title
	}
	if what == "" {
		what = "a running process"
	}
	return fmt.Sprintf("%s (%s)", p.PaneID, what)
}

// cascadeRisk says why closing this whole workspace would take others with
// it, or "".
func cascadeRisk(ws Workspace, all []Workspace) string {
	if !isPrimary(ws) {
		return ""
	}
	linked := linkedSiblings(ws, all)
	if len(linked) == 0 {
		return ""
	}
	return fmt.Sprintf("%s (%s) is the herdr group primary of %d open worktree workspace(s) (%s); closing it would close them too",
		ws.WorkspaceID, ws.Label, len(linked), labels(linked))
}

// closeContainer closes an owned tab (its workspace too, when it is the last
// tab) or workspace.
func closeContainer(c *container, dryRun bool) (string, error) {
	n := len(c.panes)
	if c.kind == "tab" && c.ws.TabCount > 1 {
		if !dryRun {
			if err := herdr(nil, "tab", "close", c.id); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("closed tab %s (%s) with %d pane(s)", c.id, c.label, n), nil
	}
	if !dryRun {
		if err := herdr(nil, "workspace", "close", c.ws.WorkspaceID); err != nil { // refuses for a primary with siblings
			return "", err
		}
	}
	if c.kind == "tab" {
		return fmt.Sprintf("closed tab %s (%s) with %d pane(s), and its workspace %s (%s)", c.id, c.label, n, c.ws.WorkspaceID, c.ws.Label), nil
	}
	return fmt.Sprintf("closed workspace %s (%s) with %d pane(s)", c.ws.WorkspaceID, c.ws.Label, n), nil
}

type plannedWT struct {
	info          wtInfo
	merge, target string
}

func doneOne(target string, o doneOpts) error {
	pfx := ""
	if o.dryRun {
		pfx = "[dry-run] "
	}
	isPath := strings.Contains(target, "/")
	var rec *Record
	var agent *Agent
	var pane *Pane
	if !isPath {
		var err error
		if rec, agent, pane, err = findTarget(target); err != nil {
			return err
		}
	}
	ps, err := panes()
	if err != nil {
		return err
	}
	all, err := workspaces()
	if err != nil {
		return err
	}
	ags, err := agents()
	if err != nil {
		return err
	}
	var cont *container
	if rec != nil {
		if cont, err = ownedContainer(rec, ps, all); err != nil {
			return err
		}
	}
	if !isPath && rec == nil && agent == nil && pane == nil {
		say("%s: nothing to tear down (no live agent, pane or dispatch record)", target)
		return nil
	}

	// ---- checks; nothing is changed until all of them pass
	closing := map[string]bool{}
	inContainer := map[string]bool{}
	if cont != nil {
		for _, p := range cont.panes {
			closing[p.PaneID], inContainer[p.PaneID] = true, true
		}
	}
	if pane != nil {
		closing[pane.PaneID] = true
	}
	if cp := callerPane(); cp != "" && closing[cp] {
		return refuse("%s: %s is the pane you are running in, and done would close it", target, cp)
	}
	if agent != nil && (agent.AgentStatus == "working" || agent.AgentStatus == "blocked") && !o.force {
		hint := ""
		if agent.AgentStatus == "blocked" {
			hint = " — `herdr agent read` it"
		}
		return refuse("%s is still %s%s; --force to tear it down anyway", target, agent.AgentStatus, hint)
	}
	if cont != nil {
		// Everything in the agent's own tab/workspace is the agent's; close it
		// all, but not a pane still running something unless told to.
		var busy []string
		for _, p := range cont.panes {
			if (pane == nil || p.PaneID != pane.PaneID) && !paneIsBlankShell(p) {
				busy = append(busy, paneDesc(p))
			}
		}
		if len(busy) > 0 && !o.force {
			return refuse("%s's %s %s also holds %s, not an idle shell prompt; stop it, or --force to close it too",
				target, cont.kind, cont.id, strings.Join(busy, ", "))
		}
		if cont.kind == "workspace" || cont.ws.TabCount == 1 {
			if risk := cascadeRisk(cont.ws, all); risk != "" {
				return refuse("%s: %s. Tear those down first.", target, risk)
			}
		}
	}

	var wts []wtInfo
	var paths []string
	if isPath {
		paths = []string{target}
	} else {
		for _, w := range recWorktrees(rec) {
			paths = append(paths, w.Path)
		}
	}
	if len(paths) == 0 && pane != nil {
		root, err := worktreeRoot()
		if err != nil {
			return err
		}
		for _, c := range []string{pane.ForegroundCwd, pane.Cwd} {
			if c == "" {
				continue
			}
			info, err := worktreeInfo(c)
			if err != nil {
				return err
			}
			if info != nil && under(info.top, root) {
				paths = []string{info.top}
				break
			}
		}
	}
	for _, wp := range paths {
		info, err := worktreeInfo(wp)
		if err != nil {
			return err
		}
		switch {
		case info != nil:
			wts = append(wts, *info)
		case isPath:
			return refuse("%s is not a linked git worktree", target)
		default:
			say("%s: worktree %s already gone", target, wp)
		}
	}
	if o.keepWorktree {
		wts = nil
	}

	me := ""
	if rec != nil {
		me = rec.Name
	}
	claims := worktreeClaims(ags, ps)
	var plan []plannedWT
	for _, w := range wts {
		var owners []string
		for _, c := range claims[norm(w.top)] {
			if c.live && c.name != me {
				owners = append(owners, c.name)
			}
		}
		if len(owners) > 0 {
			return refuse("worktree %s belongs to live agent %s — `bowery done %s` tears it down with that agent",
				w.top, strings.Join(owners, ", "), owners[0])
		}
		if others := occupants(w.top, ps, closing); len(others) > 0 {
			ids := make([]string, len(others))
			for i, p := range others {
				ids[i] = p.PaneID
			}
			return refuse("worktree %s is still in use by %s — close those first, or pass --keep-worktree to close only this agent's pane",
				w.top, strings.Join(ids, ", "))
		}
		ok, err := fetch(w.repo)
		if err != nil {
			return err
		}
		if !ok {
			warn("could not fetch origin in %s; checking against the last fetch", w.repo)
		}
		ref, err := defaultBranch(w.repo)
		if err != nil {
			return err
		}
		d, err := dirty(w.top, true)
		if err != nil {
			return err
		}
		if d && !o.force {
			return refuse("worktree %s has uncommitted or untracked files; --force to discard them, --keep-worktree to leave it", w.top)
		}
		merge := ""
		if ref != "" {
			rev := w.branch
			if rev == "" {
				rev = "HEAD"
			}
			if merge, err = mergedInto(w.top, rev, ref); err != nil {
				return err
			}
		}
		if merge == "" && !o.force {
			what := w.branch
			if what == "" {
				what = "HEAD of " + w.top
			}
			into := ref
			if into == "" {
				into = "any default branch"
			}
			return refuse("%s is not merged into %s; --force removes the checkout and keeps the branch, --keep-worktree leaves both", what, into)
		}
		plan = append(plan, plannedWT{w, merge, ref})
	}

	// ---- teardown
	if cont != nil {
		msg, err := closeContainer(cont, o.dryRun)
		if err != nil {
			return err
		}
		say("%s%s: %s", pfx, target, msg)
	}
	switch {
	case pane != nil && !inContainer[pane.PaneID]:
		msg, err := safeClosePane(pane.PaneID, o.dryRun)
		if err != nil {
			return err
		}
		say("%s%s: %s", pfx, target, msg)
	case pane == nil && cont == nil && agent == nil && !isPath:
		say("%s: pane already gone", target)
	}

	var repos []string
	for _, p := range plan {
		w := p.info
		if !o.dryRun {
			// The pane's shell can outlive the close by a moment.
			for i := 0; i < 20; i++ {
				now, err := panes()
				if err != nil || len(occupants(w.top, now, nil)) == 0 {
					break
				}
				time.Sleep(250 * time.Millisecond)
			}
			args := []string{"worktree", "remove"}
			if o.force {
				args = append(args, "--force")
			}
			if _, err := git(w.repo, append(args, w.top)...); err != nil {
				return err
			}
		}
		say("%s%s: removed worktree %s", pfx, target, w.top)
		if w.branch != "" && !o.keepBranch {
			if p.merge != "" {
				if !o.dryRun {
					if _, err := git(w.repo, "branch", "-D", w.branch); err != nil {
						return err
					}
				}
				how := "merged"
				if p.merge != "ancestor" {
					how = "content already in"
				}
				say("%s%s: deleted branch %s (%s %s)", pfx, target, w.branch, how, p.target)
			} else {
				say("%s: kept branch %s (not merged into %s); `git -C %s branch -D %s` to drop it", target, w.branch, p.target, w.repo, w.branch)
			}
		}
		if !contains(repos, w.repo) {
			repos = append(repos, w.repo)
		}
	}
	if len(repos) == 0 && rec != nil && rec.Repo != "" {
		repos = append(repos, rec.Repo)
	}
	for _, repo := range repos {
		if err := closeStrayPrimary(repo, o.dryRun); err != nil {
			return err
		}
		if err := ffMain(repo, o.dryRun); err != nil {
			return err
		}
	}
	if o.dryRun {
		return nil
	}
	if !isPath {
		n := me
		if n == "" && agent != nil {
			n = agent.Name
		}
		if n == "" {
			n = target
		}
		dropRecord(n)
	} else if len(wts) > 0 {
		return forgetWorktree(norm(wts[0].top))
	}
	return nil
}

func strayPrimaryVerdict(ws Workspace, all []Workspace, ps []Pane) string {
	if ws.TabCount != 1 || ws.PaneCount != 1 {
		return fmt.Sprintf("%d panes in %d tabs", ws.PaneCount, ws.TabCount)
	}
	if ws.Label != ws.Worktree.RepoName {
		return fmt.Sprintf("labelled '%s', not the repo name herdr gives the one it spawns", ws.Label)
	}
	var pane *Pane
	for i := range ps {
		if ps[i].WorkspaceID == ws.WorkspaceID {
			pane = &ps[i]
			break
		}
	}
	if pane == nil {
		return "no pane found"
	}
	if cp := callerPane(); cp != "" && pane.PaneID == cp {
		return "it is the pane you are running in"
	}
	if !paneIsBlankShell(*pane) {
		return "its pane is not an idle shell prompt"
	}
	if sibs := groupOf(ws, all); len(sibs) > 0 {
		return fmt.Sprintf("%d %s (%s); closing it would take them down", len(sibs), siblingsNote, labels(sibs))
	}
	return ""
}

const siblingsNote = "worktree workspace(s) of the repo still open"

func closeStrayPrimary(repo string, dryRun bool) error {
	pfx := ""
	if dryRun {
		pfx = "[dry-run] "
	}
	all, err := workspaces()
	if err != nil {
		return err
	}
	ps, err := panes()
	if err != nil {
		return err
	}
	key := repo + "/.git"
	for _, ws := range all {
		if !isPrimary(ws) || ws.Worktree.RepoKey != key {
			continue
		}
		if why := strayPrimaryVerdict(ws, all, ps); why != "" {
			say("left repo-root workspace %s (%s) open: %s", ws.WorkspaceID, ws.Label, why)
			continue
		}
		if !dryRun {
			if err := herdr(nil, "workspace", "close", ws.WorkspaceID); err != nil { // refuses if a sibling appeared meanwhile
				return err
			}
		}
		say("%sclosed blank repo-root workspace %s (%s)", pfx, ws.WorkspaceID, ws.Label)
	}
	return nil
}

// ffMain fast-forwards the main checkout when it is clean and on the default
// branch.
func ffMain(repo string, dryRun bool) error {
	pfx := ""
	if dryRun {
		pfx = "[dry-run] "
	}
	if o, err := hasOrigin(repo); err != nil || !o {
		return err
	}
	target, err := defaultBranch(repo)
	if err != nil || !strings.HasPrefix(target, "origin/") {
		return err
	}
	want := strings.TrimPrefix(target, "origin/")
	cur, _, err := gitMaybe(repo, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		return err
	}
	if cur != want {
		on := cur
		if on == "" {
			on = "a detached HEAD"
		}
		say("main checkout %s not fast-forwarded: on %s, not %s", repo, on, want)
		return nil
	}
	if d, err := dirty(repo, false); err != nil || d {
		if d {
			say("main checkout %s not fast-forwarded: uncommitted changes", repo)
		}
		return err
	}
	old, err := git(repo, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	nw, err := git(repo, "rev-parse", target)
	if err != nil || old == nw {
		return err
	}
	if _, ok, err := gitMaybe(repo, "merge-base", "--is-ancestor", "HEAD", target); err != nil || !ok {
		if err == nil {
			say("main checkout %s not fast-forwarded: %s has commits %s doesn't", repo, cur, target)
		}
		return err
	}
	if !dryRun {
		if _, err := git(repo, "merge", "-q", "--ff-only", target); err != nil {
			return err
		}
	}
	say("%sfast-forwarded %s %s %s..%s", pfx, repo, cur, old[:7], nw[:7])
	return nil
}

func cmdDone(argv []string) {
	explicit, argv := popMachine(argv)
	var o doneOpts
	var targets []string
	for _, a := range argv {
		switch a {
		case "--force":
			o.force = true
		case "--keep-worktree":
			o.keepWorktree = true
		case "--keep-branch":
			o.keepBranch = true
		case "--dry-run":
			o.dryRun = true
		default:
			if strings.HasPrefix(a, "--") {
				die("done: unknown option %s", a)
			}
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		die("done: which agent?")
	}
	rc := 0
	for _, t := range targets {
		err := pickMachine(t, explicit)
		if err == nil && strings.Contains(t, "/") {
			t, err = hostPath(t)
		}
		if err == nil {
			var unlock func()
			if unlock, err = doneLocks(t); err == nil {
				err = doneOne(t, o)
				unlock()
			}
		}
		var rf *Refused
		switch {
		case err == nil:
		case errors.As(err, &rf):
			warn("done %s: refused, nothing changed: %v", t, err)
			rc = 1
		default:
			where := ""
			if machine != "" {
				where = " (on " + machine + ")"
			}
			warn("done %s%s: %v", t, where, err)
			rc = 1
		}
	}
	os.Exit(rc)
}

// doneNames is every agent name done's target reaches: the record it
// resolves to as doneOne resolves it (by name, or a pane by its terminal),
// the live agent's name, and for a path every record claiming it, since
// done rewrites each of those.
func doneNames(target string) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n != "" && nameRE.MatchString(n) && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if strings.Contains(target, "/") {
		for _, r := range allRecords() {
			for _, w := range recWorktrees(r) {
				if norm(w.Path) == target {
					add(r.Name)
				}
			}
		}
	} else {
		rec, agent, _, err := findTarget(target)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			add(rec.Name)
		}
		if agent != nil {
			add(agent.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// doneLocks takes the name lock of every agent done's target reaches, so a
// resume, dispatch or adopt of one of them, or a list refreshing its
// record, can't run under it. A name with a record has its lock made if
// need be; one without only has its lock taken if the file is there (a
// dispatch of it may be mid-way), so a typo leaves nothing behind. Each is
// waited for briefly, since a list's refresh holds one for an instant. The
// target is resolved again once they're held: if it now reaches another
// agent, it's refused.
func doneLocks(target string) (func(), error) {
	names, err := doneNames(target)
	if err != nil {
		return nil, err
	}
	var held []func()
	release := func() {
		for _, u := range held {
			u()
		}
	}
	for _, n := range names {
		create := loadRecord(n) != nil
		var unlock func()
		busy := true
		for try := 0; try < 10 && busy; try++ {
			if try > 0 {
				time.Sleep(200 * time.Millisecond)
			}
			if unlock, busy, err = lockName(n, create); err != nil {
				release()
				return nil, err
			}
		}
		if busy {
			release()
			return nil, refuse("a resume, dispatch or adopt of %s is running; try again when it has finished", n)
		}
		held = append(held, unlock)
	}
	again, err := doneNames(target)
	if err != nil {
		release()
		return nil, err
	}
	if strings.Join(again, " ") != strings.Join(names, " ") {
		release()
		return nil, refuse("%s changed while it was being checked (it reached %s, now %s); try again",
			target, strings.Join(names, ", "), strings.Join(again, ", "))
	}
	return release, nil
}

const adoptUsage = "adopt <name> [--launcher CMD] [--gated|--ungated] [--machine LABEL] [--] [<worktree-path>...]"

// cmdAdopt records more worktrees as an agent's, and/or gives a live agent
// what resume needs to bring it back (its session and folder, the launcher
// and permission mode given). Everything is checked before the record is
// written, and only then is anything said. It takes the agent's name lock
// (so it waits for no one: a resume or dispatch under way refuses it).
func cmdAdopt(argv []string) {
	explicit, argv := popMachine(argv)
	var name, launcher, gated string
	var paths []string
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--":
			for _, rest := range argv[i+1:] {
				if name == "" {
					name = rest
				} else {
					paths = append(paths, rest)
				}
			}
			i = len(argv)
		case a == "--launcher" || strings.HasPrefix(a, "--launcher="):
			v, used := launcherArg(argv, i)
			launcher, i = v, i+used
		case a == "--gated" || a == "--ungated":
			if gated != "" && gated != a[2:] {
				die("adopt: --gated and --ungated contradict each other")
			}
			gated = a[2:]
		case strings.HasPrefix(a, "-"):
			die("adopt: unknown option %s (a path starting with - goes after --) (%s)", a, adoptUsage)
		case name == "":
			name = a
		default:
			paths = append(paths, a)
		}
	}
	if name == "" {
		die("adopt: which agent? (%s)", adoptUsage)
	}
	if !nameRE.MatchString(name) {
		die("agent name must be lowercase/digits/-/_ (got '%s')", name)
	}
	if len(paths) == 0 && launcher == "" && gated == "" {
		die("adopt: nothing to adopt: give worktree paths, --launcher or --gated|--ungated (%s)", adoptUsage)
	}
	if err := pickMachine(name, explicit); err != nil {
		die("adopt: %v", herdrMsg(err))
	}
	unlock, held, err := nameLock(name)
	if err != nil {
		die("adopt: %v", err)
	}
	if held {
		die("adopt %s: refused, nothing changed: a resume or dispatch of %s is running", name, name)
	}
	defer unlock()

	// ---- checks; nothing is written until all of them pass
	rec := loadRecord(name)
	forResume := launcher != "" || gated != ""
	var live *Agent
	var livePane Pane
	if rec == nil || forResume {
		ags, err := agents()
		if err != nil {
			die("%v", herdrMsg(err))
		}
		var found []string
		for i := range ags {
			if ags[i].Name == name {
				live = &ags[i]
				found = append(found, ags[i].PaneID)
			}
		}
		if len(found) > 1 {
			die("adopt %s: refused, nothing changed: more than one live agent is named %s (%s); rename all but one first",
				name, name, strings.Join(found, ", "))
		}
		if live != nil {
			if livePane, err = paneGet(live.PaneID); err != nil {
				die("%v", herdrMsg(err))
			}
		}
	}
	fresh := rec == nil // a record adopt writes new, from the live agent
	if rec == nil {
		if live == nil {
			die("adopt: no dispatch record and no live agent named %s", name)
		}
		snd := live.Tokens["sound"]
		if snd == "" {
			snd = "on"
		}
		sock := socketPath()
		rec = &Record{Name: name, Socket: &sock, Placement: "adopted", Kind: live.Kind, Started: nowISO(),
			PaneID: livePane.PaneID, TerminalID: livePane.TerminalID, WorkspaceID: livePane.WorkspaceID, TabID: livePane.TabID, Sound: snd}
		if machine != "" {
			r, err := remote()
			if err != nil {
				die("%v", err)
			}
			rec.Socket, rec.Machine, rec.Host, rec.As = nil, machine, r.host, as
		}
	}
	var said []string
	if forResume {
		if rec.Kind == "" && live != nil {
			rec.Kind = live.Kind // an older record, or a new one: the agent says
		}
		if rec.Kind != "claude" {
			die("adopt: --launcher and --gated|--ungated are for claude agents (%s is %q)", name, rec.Kind)
		}
		// Records from before 0.3.0 have no folder of their own, so their
		// gated says nothing: the mode must be given. A newer record's mode
		// is what it was started with, and adopt never widens it.
		old := !fresh && rec.Cwd == ""
		if (fresh || old) && gated == "" {
			what := "say how it was started"
			if old {
				what = "its record is older than bowery 0.3.0 and doesn't say how it was started; say"
			}
			die("adopt %s: %s: --gated (permission prompts on) or --ungated", name, what)
		}
		if !fresh && !old && rec.Gated && gated == "ungated" {
			die("adopt %s: refused, nothing changed: its record says it was started --gated; adopt won't widen that "+
				"(resume --ungated does, for one start)", name)
		}
		// An existing record without a session or a folder is filled from
		// its live agent, both from the same one, and only when that agent
		// is in the record's own terminal (or, for a record that has none,
		// its own pane): a name alone can't say which conversation is its.
		if !fresh && (rec.Session == "" || rec.Cwd == "") {
			if live == nil {
				die("adopt %s: refused, nothing changed: its record has no session or folder, and no live agent is named %s "+
					"to take them from; bowery resume %s --fresh starts it in a new conversation", name, name, name)
			}
			switch {
			case rec.TerminalID != "" && livePane.TerminalID != rec.TerminalID:
				die("adopt %s: refused, nothing changed: the live %s (in %s) isn't in its record's terminal %s; "+
					"bowery done %s, then adopt the live one", name, name, livePane.PaneID, rec.TerminalID, name)
			case rec.TerminalID == "" && rec.PaneID == "":
				die("adopt %s: refused, nothing changed: its record names no pane or terminal to check the live %s against; "+
					"bowery done %s, then adopt the live one", name, name, name)
			case rec.TerminalID == "" && livePane.PaneID != rec.PaneID:
				die("adopt %s: refused, nothing changed: the live %s (in %s) isn't in its record's pane %s; "+
					"bowery done %s, then adopt the live one", name, name, livePane.PaneID, rec.PaneID, name)
			}
			rec.Session, rec.Cwd = "", ""
		}
		if rec.Session == "" {
			s := live.SessionID()
			if s == "" {
				die("adopt %s: refused, nothing changed: herdr reports no session for it, so resume couldn't use the record%s", name, oldHerdrNote())
			}
			rec.Session = s
		}
		if rec.Cwd == "" {
			// Where the running claude is (what it filed its session under),
			// not the shell it was started from.
			rec.Cwd = livePane.ForegroundCwd
			if rec.Cwd == "" {
				rec.Cwd = livePane.Cwd
			}
			if rec.Cwd == "" {
				rec.Cwd = live.Cwd
			}
			if rec.Cwd == "" {
				die("adopt %s: refused, nothing changed: herdr doesn't say which folder it runs in", name)
			}
		}
		if launcher != "" {
			rec.Launcher = launcher
			said = append(said, "launcher "+launcher)
		}
		if gated != "" {
			rec.Gated = gated == "gated"
			said = append(said, map[bool]string{true: "gated (permission prompts on)", false: "ungated"}[rec.Gated])
		}
		said = append(said, fmt.Sprintf("session %s, in %s: bowery resume %s can bring it back", rec.Session, rec.Cwd, name))
	}
	have := recWorktrees(rec)
	for _, path := range paths {
		w, err := linkedWorktree(path)
		if err != nil {
			die("adopt: %v", err)
		}
		dup := false
		for _, h := range have {
			if norm(h.Path) == norm(w.Path) {
				dup = true
			}
		}
		if dup {
			said = append(said, "already has "+w.Path)
			continue
		}
		have = append(have, w)
		b := w.Branch
		if b == "" {
			b = "detached"
		}
		said = append(said, fmt.Sprintf("adopted %s [%s]", w.Path, b))
	}
	rec.Worktrees = have
	if err := writeRecord(rec); err != nil {
		die("could not write the dispatch record: %v", err)
	}
	for _, l := range said {
		say("%s: %s", name, l)
	}
}
