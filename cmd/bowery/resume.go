package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// resumeNotice is the first thing a resumed agent is told. It may already
// have done part of its job, some of it costly or impossible to undo.
const resumeNotice = "You were resumed: check what you already did before redoing anything. " +
	"Anything that costs money or can't be undone (paid work you placed, a message you sent, a release you published) " +
	"is checked for, not done again."

type resumeOpts struct {
	placement, timeout string
	dryRun             bool
	prompt             []string
}

func cmdResume(argv []string) {
	explicit, argv := popMachine(argv)
	if len(argv) == 0 || strings.HasPrefix(argv[0], "-") {
		die("resume: which agent? (bowery resume <name> [--swarm|--workspace] [--dry-run] [-- <prompt>])")
	}
	name, argv := argv[0], argv[1:]
	if !nameRE.MatchString(name) {
		die("agent name must be lowercase/digits/-/_ (got '%s')", name)
	}
	o := resumeOpts{timeout: "60000"}
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; a {
		case "--swarm", "--workspace":
			o.placement = a[2:]
		case "--dry-run":
			o.dryRun = true
		case "--timeout":
			if i+1 >= len(argv) {
				die("--timeout needs a value")
			}
			o.timeout = argv[i+1]
			i++
		case "--":
			o.prompt = argv[i+1:]
			i = len(argv)
		default:
			die("resume: unknown option %s", a)
		}
	}
	if err := pickMachine(name, explicit); err != nil {
		die("resume: %v", herdrMsg(err))
	}
	err := resumeOne(name, o)
	var rf *Refused
	switch {
	case err == nil:
	case errors.As(err, &rf):
		die("resume %s: refused, nothing changed: %v", name, err)
	default:
		where := ""
		if machine != "" {
			where = " (on " + machine + ")"
		}
		die("resume %s%s: %v", name, where, herdrMsg(err))
	}
}

// landing is where a resumed agent will start.
type landing struct {
	placement string // "pane" (its own terminal, reused), "split" (in its own tab), "swarm", "workspace"
	pane      string // reused pane, or the pane to split
	why       string
}

func resumeOne(name string, o resumeOpts) error {
	rec := loadRecord(name)
	if rec == nil {
		where := ""
		if machine != "" {
			where = " on " + machine
		}
		return refuse("no dispatch record for %s%s; resume only brings back an agent bowery dispatched", name, where)
	}

	// ---- checks; nothing is changed until all of them pass
	if rec.Kind != "" && rec.Kind != "claude" {
		return refuse("%s is a %s agent; resume knows how to resume claude sessions only", name, rec.Kind)
	}
	ags, err := agents()
	if err != nil {
		return err
	}
	ps, err := panes()
	if err != nil {
		return err
	}
	all, err := workspaces()
	if err != nil {
		return err
	}
	refreshSessions(ags)
	if r := loadRecord(name); r != nil {
		rec = r
	}
	for _, a := range ags {
		if a.Name == name {
			return refuse("%s is still alive (%s, in %s); nothing to resume", name, a.AgentStatus, a.PaneID)
		}
	}
	if rec.Session == "" {
		return refuse("%s's record has no session id (dispatched by an older bowery, and never seen alive since); "+
			"dispatch it again instead", name)
	}
	for _, a := range ags {
		if a.SessionID() == rec.Session {
			who := a.Name
			if who == "" {
				who = "an unnamed agent"
			}
			return refuse("%s's session %s is live in %s (%s); one conversation can't run twice", name, rec.Session, a.PaneID, who)
		}
	}
	cwd := rec.Cwd
	if cwd == "" {
		cwd = rec.Worktree
	}
	if cwd == "" {
		return refuse("%s's record doesn't say which directory it ran in (an older record without a worktree); "+
			"dispatch it again instead", name)
	}
	if d, err := isDir(cwd); err != nil {
		return err
	} else if !d {
		return refuse("%s ran in %s, which is gone", name, cwd)
	}
	for _, w := range recWorktrees(rec) {
		ex, err := exists(w.Path)
		if err != nil {
			return err
		}
		if !ex {
			return refuse("%s's worktree %s is gone; its work can't be resumed there", name, w.Path)
		}
	}
	l, err := planLanding(rec, o, ags, ps, all)
	if err != nil {
		return err
	}
	if l.pane != "" {
		if tag, err := handsOff(l.pane); err != nil {
			return err
		} else if tag != "" {
			return refuse("%s holds a process carrying %s, which belongs to another tool; not starting %s there", l.pane, tag, name)
		}
	}

	at, err := checkoutAt(cwd)
	if err != nil {
		return err
	}

	agentArgs := []string{"--"}
	if !rec.Gated {
		agentArgs = append(agentArgs, "--dangerously-skip-permissions")
	}
	if rec.Model != "" {
		agentArgs = append(agentArgs, "--model", rec.Model)
	}
	agentArgs = append(agentArgs, "--resume", rec.Session)

	where := l.why
	if machine != "" {
		where += " on " + machine
	}
	if rec.As != "" {
		where += " as " + rec.As
	}
	if o.dryRun {
		say("[dry-run] would resume %s (session %s) in %s, %s", name, rec.Session, cwd, where)
		return nil
	}

	// ---- start
	so := startOpts{placement: l.placement, dir: "right", kind: "claude", timeout: o.timeout, cwd: cwd}
	from := ""
	switch l.placement {
	case "pane":
		so.pane = l.pane
		// claude files a session under the directory it starts in.
		if cur, err := paneGet(l.pane); err == nil && norm(cur.Cwd) != norm(cwd) {
			if err := herdr(nil, "pane", "run", l.pane, "cd "+shq(cwd)); err != nil {
				return err
			}
		}
	case "split":
		from = l.pane
	}
	scratch := &Record{}
	var also []RecWorktree
	m := &made{}
	pane, err := startAgent(name, so, from, cwd, agentArgs, scratch, &also, m)
	if err != nil {
		undoStart(m)
		return err
	}
	sound := rec.Sound
	if sound == "" {
		sound = "off"
	}
	if err := setSound(pane, sound); err != nil {
		return err
	}
	msg := resumeNotice
	if at != "" {
		msg += " " + at
	}
	if len(o.prompt) > 0 {
		msg += "\n\n" + strings.Join(o.prompt, " ")
	}
	if err := herdr(nil, "agent", "prompt", name, msg); err != nil {
		return err
	}

	p, err := paneGet(pane)
	if err != nil {
		return err
	}
	rec.PaneID, rec.TerminalID, rec.WorkspaceID, rec.TabID = p.PaneID, p.TerminalID, p.WorkspaceID, p.TabID
	switch l.placement {
	case "swarm":
		rec.Placement, rec.CreatedPane, rec.Owns = "swarm", true, &Owns{Kind: "tab", ID: p.TabID}
	case "workspace":
		rec.Placement, rec.CreatedPane, rec.Owns = "workspace", true, &Owns{Kind: "workspace", ID: p.WorkspaceID}
	}
	if s := sessionIn(p.PaneID); s != "" {
		rec.Session = s
	}
	rec.Cwd = cwd
	rec.Resumed, rec.Resumes = nowISO(), rec.Resumes+1
	if err := writeRecord(rec); err != nil {
		return fail("resumed, but could not write the dispatch record: %v", err)
	}
	out := []kv{{"name", name}, {"pane_id", p.PaneID}, {"workspace_id", p.WorkspaceID}, {"tab_id", p.TabID},
		{"sound", sound}, {"session", rec.Session}, {"resumes", fmt.Sprint(rec.Resumes)}}
	if machine != "" {
		out = append(out, kv{"machine", machine})
	}
	if rec.As != "" {
		out = append(out, kv{"as", rec.As})
	}
	fmt.Println(jsonLine(out))
	return nil
}

// planLanding picks where the agent comes back, without changing anything:
// its own terminal if that is still open at a blank shell; else a new pane in
// its own tab or workspace if that is still open; else a new place of the
// kind it had. It never lands in a pane that isn't the agent's.
func planLanding(rec *Record, o resumeOpts, ags []Agent, ps []Pane, all []Workspace) (landing, error) {
	if rec.TerminalID != "" && o.placement == "" {
		for _, p := range ps {
			if p.TerminalID != rec.TerminalID {
				continue
			}
			for _, a := range ags {
				if a.PaneID == p.PaneID {
					who := a.Name
					if who == "" {
						who = "an unnamed " + a.Kind + " agent"
					}
					return landing{}, refuse("%s's pane %s now holds %s; not starting it in someone else's pane", rec.Name, p.PaneID, who)
				}
			}
			if !paneIsBlankShell(p) {
				return landing{}, refuse("%s's pane %s is running %s, not sitting at a shell prompt; stop it, or close the pane",
					rec.Name, p.PaneID, paneDesc(p))
			}
			return landing{placement: "pane", pane: p.PaneID, why: "its own pane " + p.PaneID}, nil
		}
	}
	if o.placement == "" {
		if c := ownedContainer(rec, ps, all); c != nil && len(c.panes) > 0 {
			return landing{placement: "split", pane: c.panes[0].PaneID,
				why: fmt.Sprintf("a new pane in its %s %s", c.kind, c.id)}, nil
		}
	}
	pl := o.placement
	if pl == "" {
		pl = "swarm"
		if rec.Placement == "workspace" {
			pl = "workspace"
		}
	}
	if pl == "workspace" {
		return landing{placement: "workspace", why: "a new workspace " + rec.Name}, nil
	}
	return landing{placement: "swarm", why: fmt.Sprintf("a new tab %s in %s", rec.Name, cfg.Swarm)}, nil
}

// resumable says whether resume could bring a dead record's agent back:
// it has a session, and its directory and worktrees are still there.
func resumable(r *Record) (bool, error) {
	if r.Session == "" || (r.Kind != "" && r.Kind != "claude") {
		return false, nil
	}
	cwd := r.Cwd
	if cwd == "" {
		cwd = r.Worktree
	}
	if cwd == "" {
		return false, nil
	}
	if d, err := isDir(cwd); err != nil || !d {
		return false, err
	}
	for _, w := range recWorktrees(r) {
		if ex, err := exists(w.Path); err != nil || !ex {
			return false, err
		}
	}
	return true, nil
}

// handsOff names the first hands_off_env variable carried by the shell or a
// foreground process of the pane, or "". herdr's process-info gives the
// pids; their environments are read where the pane runs (/proc, or ps -E
// where there is no /proc).
func handsOff(pane string) (string, error) {
	if len(cfg.HandsOffEnv) == 0 {
		return "", nil
	}
	var r struct {
		ProcessInfo struct {
			ShellPID  int `json:"shell_pid"`
			Processes []struct {
				PID int `json:"pid"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := herdr(&r, "pane", "process-info", "--pane", pane); err != nil {
		return "", err
	}
	pids := []int{r.ProcessInfo.ShellPID}
	for _, p := range r.ProcessInfo.Processes {
		pids = append(pids, p.PID)
	}
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		env, err := procEnv(pid)
		if err != nil {
			return "", err
		}
		for _, v := range cfg.HandsOffEnv {
			if regexp.MustCompile(`(^|[\s\x00])` + regexp.QuoteMeta(v) + `=`).MatchString(env) {
				return v, nil
			}
		}
	}
	return "", nil
}

// procEnv is a process's environment, as text, on the current server. A
// process that is gone or not ours to read has none.
func procEnv(pid int) (string, error) {
	script := fmt.Sprintf(`if [ -r /proc/%[1]d/environ ]; then tr '\0' '\n' < /proc/%[1]d/environ; else ps -wwE -o command= -p %[1]d 2>/dev/null; fi; true`, pid)
	if machine != "" {
		rc, out, err := sshSh(script)
		if err != nil {
			return "", err
		}
		if rc != 0 {
			return "", fail("reading the environment of pid %d on %s: %s", pid, machine, strings.TrimSpace(out))
		}
		return out, nil
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
		return string(b), nil
	}
	out, _ := exec.Command("sh", "-c", script).Output()
	return string(out), nil
}

// checkoutAt says where the agent's checkout is now ("Your checkout X is on
// branch B at C."), so it can compare with git log before redoing anything;
// "" when its directory isn't in a git checkout.
func checkoutAt(dir string) (string, error) {
	sha, ok, err := gitOut(dir, "rev-parse", "--short", "HEAD")
	if err != nil || !ok {
		return "", err
	}
	branch, ok, err := gitOut(dir, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	if !ok || branch == "" {
		return fmt.Sprintf("Your checkout %s is at %s (detached); compare with git log.", dir, sha), nil
	}
	return fmt.Sprintf("Your checkout %s is on %s at %s; compare with git log.", dir, branch, sha), nil
}
