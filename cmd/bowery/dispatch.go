package main

import (
	"os/exec"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const trustText = "trust this folder"

type startOpts struct {
	placement, dir, pane, from, cwd, sound, kind, model, timeout string
	gated, wait                                                  bool
	branch, base                                                 string
	also, prompt                                                 []string
	machine, as                                                  string
}

func parseStart(argv []string) startOpts {
	o := startOpts{placement: "swarm", dir: "right", sound: "off", kind: "claude", timeout: "60000"}
	valued := map[string]*string{
		"--pane": &o.pane, "--from": &o.from, "--cwd": &o.cwd, "--kind": &o.kind, "--model": &o.model,
		"--timeout": &o.timeout, "--worktree": &o.branch, "--base": &o.base, "--machine": &o.machine, "--as": &o.as,
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		next, hasNext := "", i+1 < len(argv)
		if hasNext {
			next = argv[i+1]
		}
		if dst, ok := valued[a]; ok || a == "--also-worktree" {
			if !hasNext {
				die("%s needs a value", a)
			}
			if a == "--also-worktree" {
				o.also = append(o.also, next)
			} else {
				*dst = next
			}
			if a == "--pane" {
				o.placement = "pane"
			}
			i++
			continue
		}
		switch a {
		case "--split":
			o.placement = "split"
			if next == "right" || next == "down" {
				o.dir = next
				i++
			}
		case "--swarm":
			o.placement = "swarm"
		case "--tab":
			o.placement = "tab"
		case "--workspace":
			o.placement = "workspace"
		case "--quiet":
			o.sound = "off"
		case "--loud":
			o.sound = "on"
		case "--gated":
			o.gated = true
		case "--wait":
			o.wait = true
		case "--":
			o.prompt = argv[i+1:]
			i = len(argv)
		default:
			die("unknown option %s", a)
		}
	}
	if o.base != "" && o.branch == "" {
		die("--base only goes with --worktree")
	}
	if o.branch != "" && o.placement == "pane" {
		die("--worktree makes a new checkout, so it needs a new pane: drop --pane")
	}
	if o.as != "" && o.machine == "" {
		if !nameRE.MatchString(o.as) {
			die("--as must be lowercase/digits/-/_ (got '%s')", o.as)
		}
		if cfg.IdentityCommand == "" {
			die("--as picks an identity, and this machine has no identity_command in %s; a local agent runs as you", cfg.path)
		}
	}
	if o.machine != "" {
		if o.as != "" && !nameRE.MatchString(o.as) {
			die("--as must be lowercase/digits/-/_ (got '%s')", o.as)
		}
		if o.placement == "split" || o.placement == "tab" || o.from != "" {
			die("--split, --tab and --from place the agent beside your own pane, which is local; " +
				"with --machine use --swarm (default), --workspace or --pane")
		}
		if o.branch != "" && o.cwd == "" {
			die("--worktree with --machine needs --cwd <repo on the box>")
		}
		ident := machineConfig(o.machine).IdentityCommand
		if ident != "" && o.as == "" {
			die("--machine %s needs --as <who>: it has identities (identity_command), and an agent there with none can't push or commit", o.machine)
		}
		if ident == "" && o.as != "" {
			die("--as %s: machine %s has no identity_command in %s, so there is no one to run as", o.as, o.machine, cfg.path)
		}
	}
	return o
}

func cmdDispatch(argv []string) {
	if len(argv) == 0 || strings.HasPrefix(argv[0], "-") {
		die("dispatch: which name? (bowery dispatch <name> [options] [-- <prompt>])")
	}
	name, argv := argv[0], argv[1:]
	if !nameRE.MatchString(name) {
		die("agent name must be lowercase/digits/-/_ (got '%s')", name)
	}
	o := parseStart(argv)
	if o.machine != "" {
		useMachine(o.machine, o.as)
		r, err := remote()
		if err == nil && o.cwd != "" {
			o.cwd, err = remotePath(o.cwd)
		}
		if err != nil {
			die("--machine %s: %v", machine, err)
		}
		if as != "" {
			mc := machineConfig(machine)
			rc, out, err := sshSh(identityPrefix() + "true")
			if err != nil {
				die("--machine %s: %v", machine, err)
			}
			if rc != 0 {
				have := "none"
				if lrc, l, err := sshSh(mc.IdentityCommand + " --list"); err == nil && lrc == 0 && strings.TrimSpace(l) != "" {
					have = strings.Join(strings.Fields(l), ", ")
				}
				die("--as %s: no usable identity on %s (%s); identities there: %s", as, machine, strings.TrimSpace(out), have)
			}
		}
		if o.cwd != "" {
			if d, err := isDir(o.cwd); err != nil || !d {
				hint := ""
				if o.branch != "" {
					hint = " — clone the repo there first"
				}
				die("--cwd %s does not exist on %s%s", o.cwd, machine, hint)
			}
		}
		_ = r
	} else if o.as != "" {
		// An identity on this machine's own server (a box dispatching to itself).
		useMachine("", o.as)
		if out, err := exec.Command(expandUser(cfg.IdentityCommand), o.as, "true").CombinedOutput(); err != nil {
			have := "none"
			if l, err := exec.Command(expandUser(cfg.IdentityCommand), "--list").Output(); err == nil && strings.TrimSpace(string(l)) != "" {
				have = strings.Join(strings.Fields(string(l)), ", ")
			}
			die("--as %s: no usable identity here (%s); identities: %s", o.as, strings.TrimSpace(string(out)), have)
		}
	}

	// Args handed through to the agent executable (after --). Bypassing
	// permissions is the default: a dispatched pane is unwatched, so a
	// permission prompt wedges it silently. The flag is claude's; other kinds
	// get nothing rather than a flag their executable would choke on.
	var extra []string
	if o.kind == "claude" && !o.gated {
		extra = append(extra, "--dangerously-skip-permissions")
	}
	if o.model != "" {
		if o.kind != "claude" {
			die("--model is only supported with --kind claude (got kind=%s)", o.kind)
		}
		m := o.model
		if full, ok := cfg.Models[m]; ok {
			m = full
		}
		extra = append(extra, "--model", m)
	}
	var agentArgs []string
	if len(extra) > 0 {
		agentArgs = append([]string{"--"}, extra...)
	}
	var also []RecWorktree
	for _, p := range o.also {
		w, err := linkedWorktree(p)
		if err != nil {
			die("--also-worktree: %v", err)
		}
		also = append(also, w)
	}

	from := o.from
	if from == "" && (o.placement == "split" || o.placement == "tab") {
		p, err := currentPane()
		if err != nil || p == "" {
			die("not inside a herdr pane; pass --from <pane> or --pane <pane>")
		}
		from = p
	}

	sock := socketPath()
	rec := &Record{Name: name, Socket: &sock, Placement: o.placement, Kind: o.kind, Started: nowISO()}
	here, _ := os.Getwd() // default cwd for a new pane
	if machine != "" {
		r, _ := remote()
		rec.Socket, rec.Machine, rec.Host, rec.As = nil, machine, r.host, as
		here = r.home
	} else {
		rec.As = as
	}
	m := &made{}
	pane, err := startAgent(name, o, from, here, agentArgs, rec, &also, m)
	if err != nil {
		undoStart(m)
		die("%v", err)
	}

	if err := setSound(pane, o.sound); err != nil {
		die("%v", herdrMsg(err))
	}
	if len(o.prompt) > 0 {
		args := []string{"agent", "prompt", name, strings.Join(o.prompt, " ")}
		if o.wait {
			args = append(args, "--wait")
		}
		if err := herdr(nil, args...); err != nil {
			die("%v", herdrMsg(err))
		}
	}

	p, err := paneGet(pane)
	if err != nil {
		die("%v", herdrMsg(err))
	}
	rec.PaneID, rec.TerminalID, rec.WorkspaceID, rec.TabID = p.PaneID, p.TerminalID, p.WorkspaceID, p.TabID
	rec.CreatedPane, rec.Sound, rec.Worktrees = m.pane != "", o.sound, also
	// A tab or workspace bowery made is the agent's: whatever it opens there
	// later is its to clean up. A --split or --pane sits beside panes that are
	// not, so there the agent owns only its own pane.
	switch o.placement {
	case "swarm", "tab":
		rec.Owns = &Owns{Kind: "tab", ID: p.TabID}
	case "workspace":
		rec.Owns = &Owns{Kind: "workspace", ID: p.WorkspaceID}
	}
	if err := writeRecord(rec); err != nil {
		die("could not write the dispatch record: %v", err)
	}
	out := []kv{{"name", name}, {"pane_id", p.PaneID}, {"workspace_id", p.WorkspaceID}, {"tab_id", p.TabID}, {"sound", o.sound}}
	if rec.Worktree != "" {
		out = append(out, kv{"worktree", rec.Worktree}, kv{"branch", rec.Branch})
	}
	if machine != "" {
		out = append(out, kv{"machine", machine})
	}
	if as != "" {
		out = append(out, kv{"as", as})
	}
	fmt.Println(jsonLine(out))
}

// made is what a start created, to take back if it fails.
type made struct {
	pane, worktree, branch, repo string
}

func currentPane() (string, error) {
	var r struct{ Pane Pane }
	err := herdr(&r, "pane", "current")
	return r.Pane.PaneID, err
}

func startAgent(name string, o startOpts, from, here string, agentArgs []string, rec *Record, also *[]RecWorktree, m *made) (string, error) {
	cwd := o.cwd
	if o.branch != "" {
		// A plain `git worktree add`, never `herdr worktree create`: see
		// docs/design.md for what the herdr version groups.
		src := cwd
		if src == "" {
			src = here
		}
		repo, err := mainCheckout(src)
		if err != nil {
			return "", err
		}
		if repo == "" {
			return "", fail("--worktree: %s is not in a git repository", src)
		}
		branch := o.branch
		if _, ok, err := gitMaybe(repo, "check-ref-format", "--branch", branch); err != nil || !ok {
			if err != nil {
				return "", err
			}
			return "", fail("--worktree: '%s' is not a valid branch name", branch)
		}
		if _, ok, err := gitMaybe(repo, "rev-parse", "-q", "--verify", "refs/heads/"+branch); err != nil || ok {
			if err != nil {
				return "", err
			}
			return "", fail("--worktree: branch %s already exists in %s", branch, repo)
		}
		root, err := worktreeRoot()
		if err != nil {
			return "", err
		}
		path := filepath.Join(root, filepath.Base(repo), strings.ReplaceAll(branch, "/", "-")) // as herdr names them
		if ex, err := exists(path); err != nil || ex {
			if err != nil {
				return "", err
			}
			return "", fail("--worktree: %s already exists", path)
		}
		base := o.base
		if base == "" {
			ok, err := fetch(repo)
			if err != nil {
				return "", err
			}
			if !ok {
				return "", fail("--worktree: could not fetch origin in %s; pass --base to start from a local ref", repo)
			}
			if base, err = defaultBranch(repo); err != nil {
				return "", err
			}
			if base == "" {
				base = "HEAD"
			}
		}
		if machine != "" {
			rc, out, err := sshSh(shJoin("mkdir", "-p", filepath.Dir(path)))
			if err != nil {
				return "", err
			}
			if rc != 0 {
				return "", fail("--worktree: mkdir %s on %s: %s", filepath.Dir(path), machine, strings.TrimSpace(out))
			}
		} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", fail("--worktree: %v", err)
		}
		if _, err := git(repo, "worktree", "add", "-q", "--no-track", "-b", branch, path, base); err != nil {
			return "", err
		}
		m.worktree, m.branch, m.repo = path, branch, repo
		rec.Worktree, rec.Branch, rec.Repo, rec.Base = path, branch, repo, base
		*also = append([]RecWorktree{{Path: path, Repo: repo, Branch: branch}}, *also...)
		cwd = path
	}
	var cwdArgs []string
	if cwd != "" {
		cwdArgs = []string{"--cwd", cwd}
	}
	newCwd := cwd
	if newCwd == "" {
		newCwd = here
	}

	var pane string
	switch o.placement {
	case "pane":
		pane = o.pane
	case "swarm":
		p, err := swarmTab(name, newCwd)
		if err != nil {
			return "", err
		}
		pane, m.pane = p, p
	case "split":
		var r struct{ Pane Pane }
		if err := herdr(&r, append([]string{"pane", "split", "--pane", from, "--direction", o.dir, "--no-focus"}, cwdArgs...)...); err != nil {
			return "", err
		}
		pane, m.pane = r.Pane.PaneID, r.Pane.PaneID
	case "tab":
		fp, err := paneGet(from)
		if err != nil {
			return "", err
		}
		var r struct {
			RootPane Pane `json:"root_pane"`
		}
		if err := herdr(&r, append([]string{"tab", "create", "--workspace", fp.WorkspaceID, "--label", name}, cwdArgs...)...); err != nil {
			return "", err
		}
		pane, m.pane = r.RootPane.PaneID, r.RootPane.PaneID
	default: // workspace
		var r struct {
			RootPane Pane `json:"root_pane"`
		}
		if err := herdr(&r, "workspace", "create", "--label", name, "--cwd", newCwd, "--no-focus"); err != nil {
			return "", err
		}
		pane, m.pane = r.RootPane.PaneID, r.RootPane.PaneID
	}
	if pane == "" {
		return "", fail("could not obtain a pane")
	}
	if as != "" && machineConfig(machine).IdentityCommand != "" {
		if err := become(pane, as); err != nil {
			return "", err
		}
	}

	// A fresh pane exists before its shell is ready; agent start refuses it
	// with agent_pane_busy until then. Retry that one error only.
	timeoutMS := 60000
	fmt.Sscan(o.timeout, &timeoutMS)
	for attempt := 1; ; attempt++ {
		err := herdr(nil, append([]string{"agent", "start", name, "--kind", o.kind, "--pane", pane, "--timeout", o.timeout}, agentArgs...)...)
		if err == nil {
			return pane, nil
		}
		var he *HerdrError
		if !errors.As(err, &he) {
			return "", err
		}
		if he.Code == "agent_pane_busy" && attempt < 15 {
			time.Sleep(time.Second)
			continue
		}
		if he.Code == "agent_not_ready" {
			ok, screen, terr := acceptTrustDialog(name, pane, timeoutMS)
			if terr != nil {
				return "", terr
			}
			if ok {
				return pane, nil
			}
			var lines []string
			for _, l := range strings.Split(screen, "\n") {
				if strings.TrimSpace(l) != "" {
					lines = append(lines, l)
				}
			}
			tail := strings.Join(lines, "\n")
			if len(tail) > 1200 {
				tail = tail[len(tail)-1200:]
			}
			return "", fail("agent start failed in %s: %s\n%s", pane, he.Raw, tail)
		}
		return "", fail("agent start failed in %s: %s", pane, he.Raw)
	}
}

// acceptTrustDialog answers Claude's folder-trust dialog Yes, when that is
// what is blocking and the folder is under a trusted root.
func acceptTrustDialog(name, paneID string, timeoutMS int) (bool, string, error) {
	screen, err := herdrText("agent", "read", name)
	if err != nil {
		return false, "", nil
	}
	if !strings.Contains(screen, trustText) {
		return false, screen, nil
	}
	ps, err := panes()
	if err != nil {
		return false, "", err
	}
	cwd := ""
	for _, p := range ps {
		if p.PaneID == paneID {
			cwd = p.ForegroundCwd
			if cwd == "" {
				cwd = p.Cwd
			}
		}
	}
	roots, err := trustedRoots()
	if err != nil {
		return false, "", err
	}
	trusted := false
	for _, r := range roots {
		if cwd != "" && under(cwd, r) {
			trusted = true
		}
	}
	if !trusted {
		return false, "", fail("Claude wants the folder %s trusted, which is outside %s; not answering for you", cwd, strings.Join(roots, ", "))
	}
	// The options are "No, exit" (selected) then "Yes, I trust this folder":
	// a bare Enter would quit the agent.
	if err := herdr(nil, "agent", "send-keys", name, "Down", "Enter"); err != nil {
		return false, "", err
	}
	if err := herdr(nil, "agent", "wait", name, "--until", "idle", "--timeout", fmt.Sprint(timeoutMS)); err != nil {
		return false, "", err
	}
	after, err := herdrText("agent", "read", name)
	if err != nil {
		return false, "", err
	}
	if strings.Contains(after, trustText) {
		return false, "", fail("answered the folder-trust dialog but it is still showing")
	}
	return true, "", nil
}

// swarmTab makes a new tab labelled <label> in the swarm workspace (made if
// absent) and returns its root pane. Swarm is a plain workspace rooted at
// $HOME, so herdr never treats it as a worktree group primary, and a tab's
// cwd (a worktree, say) doesn't change that. Find-or-create runs under a lock
// so two dispatches at once don't make two swarms.
func swarmTab(label, cwd string) (string, error) {
	lockName := ".swarm.lock"
	if machine != "" {
		lockName = ".swarm." + serverTag() + ".lock"
	}
	lf, err := os.OpenFile(filepath.Join(stateDir(), lockName), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fail("swarm lock: %v", err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return "", fail("swarm lock: %v", err)
	}
	all, err := workspaces()
	if err != nil {
		return "", err
	}
	ws := findSwarm(all)
	wsID, fresh := "", ""
	if ws != nil {
		wsID = ws.WorkspaceID
	} else {
		h := home()
		if machine != "" {
			r, err := remote()
			if err != nil {
				return "", err
			}
			h = r.home
		}
		var r struct {
			Workspace Workspace
			Tab       Tab
		}
		if err := herdr(&r, "workspace", "create", "--label", cfg.Swarm, "--cwd", h, "--no-focus"); err != nil {
			return "", err
		}
		wsID, fresh = r.Workspace.WorkspaceID, r.Tab.TabID
	}
	var r struct {
		RootPane Pane `json:"root_pane"`
	}
	if err := herdr(&r, "tab", "create", "--workspace", wsID, "--label", label, "--cwd", cwd, "--no-focus"); err != nil {
		return "", err
	}
	if fresh != "" {
		_ = herdr(nil, "tab", "close", fresh) // the blank shell tab a new workspace comes with
	}
	if err := swarmLast(); err != nil {
		warn("could not move %s to the bottom: %v", cfg.Swarm, err)
	}
	return r.RootPane.PaneID, nil
}

// become turns a box pane's shell into <who>'s, through the machine's
// identity command, and proves it by the variable that command sets. herdr
// starts the agent from the pane's shell, so the agent inherits that
// environment, and the identity's secrets never leave the box.
func become(pane, who string) error {
	mc := machineConfig(machine)
	for attempt := 1; ; attempt++ {
		err := herdr(nil, "pane", "run", pane, "exec "+mc.IdentityCommand+" "+shq(who))
		if err == nil {
			break
		}
		var he *HerdrError
		if errors.As(err, &he) && he.Code == "agent_pane_busy" && attempt < 15 { // shell not up yet
			time.Sleep(time.Second)
			continue
		}
		return err
	}
	const marker = "bowery-as="
	if err := herdr(nil, "pane", "run", pane, fmt.Sprintf(`printf "%s%%s\n" "$%s"`, marker, mc.IdentityEnv)); err != nil {
		return err
	}
	if err := herdr(nil, "pane", "wait-output", pane, "--match", marker+who, "--timeout", "15000"); err != nil {
		return fail("pane %s did not become %s (%s not set by %s): %v", pane, who, mc.IdentityEnv, mc.IdentityCommand, herdrMsg(err))
	}
	return herdr(nil, "pane", "run", pane, "clear")
}

// undoStart takes back what a failed start made: a half-started agent is an
// orphan.
func undoStart(m *made) {
	if m.pane != "" {
		if _, err := safeClosePane(m.pane, false); err != nil {
			warn("left pane %s open: %v", m.pane, err)
		}
	}
	if m.worktree != "" {
		_, _, _ = gitOut(m.repo, "worktree", "remove", "--force", m.worktree)
		_, _, _ = gitOut(m.repo, "branch", "-D", m.branch)
	}
}

func cmdSwarmTab(argv []string) {
	explicit, rest := popMachine(argv)
	useMachine(explicit, "")
	if len(rest) == 0 {
		die("swarm-tab: which label?")
	}
	cwd := home()
	if machine != "" {
		r, err := remote()
		if err != nil {
			die("%v", err)
		}
		cwd = r.home
	}
	switch {
	case len(rest) == 3 && rest[1] == "--cwd":
		if machine != "" {
			p, err := remotePath(rest[2])
			if err != nil {
				die("%v", err)
			}
			cwd = p
		} else {
			cwd = rest[2]
		}
	case len(rest) != 1:
		die("usage: swarm-tab <label> [--cwd DIR] [--machine LABEL]")
	}
	p, err := swarmTab(rest[0], cwd)
	if err != nil {
		die("%v", herdrMsg(err))
	}
	fmt.Println(p)
}
