package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// resumeNotice is the first thing a resumed agent is told. It may already
// have done part of its job, some of it costly or impossible to undo.
const resumeNotice = "You were resumed: check what you already did before redoing anything. " +
	"Anything that costs money or can't be undone (paid work you placed, a message you sent, a release you published) " +
	"is checked for, not done again."

type resumeOpts struct {
	placement, timeout string
	gated              string // "", "gated" or "ungated": overrides what the record says
	launcher           string // overrides the record's launcher
	noLauncher         bool   // start with herdr agent start, dropping the record's launcher
	dryRun             bool
	prompt             []string
}

const resumeUsage = "bowery resume <name> [--swarm|--workspace] [--gated|--ungated] [--launcher CMD|--no-launcher] " +
	"[--machine LABEL] [--timeout MS] [--dry-run] [-- <prompt>]"

func cmdResume(argv []string) {
	// Everything after a literal -- is the prompt, flags and all.
	var prompt []string
	for i, a := range argv {
		if a == "--" {
			argv, prompt = argv[:i], argv[i+1:]
			break
		}
	}
	explicit, argv := popMachine(argv)
	o := resumeOpts{timeout: "60000", prompt: prompt}
	name := ""
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; a {
		case "--swarm", "--workspace":
			if o.placement != "" && o.placement != a[2:] {
				die("resume: --swarm and --workspace are two different places; pick one")
			}
			o.placement = a[2:]
		case "--gated", "--ungated":
			if o.gated != "" && o.gated != a[2:] {
				die("resume: --gated and --ungated contradict each other")
			}
			o.gated = a[2:]
		case "--dry-run":
			o.dryRun = true
		case "--launcher":
			v, used := launcherArg(argv, i)
			o.launcher = v
			i += used
		case "--no-launcher":
			o.noLauncher = true
		case "--timeout":
			if i+1 >= len(argv) {
				die("--timeout needs a value")
			}
			if n, err := strconv.Atoi(argv[i+1]); err != nil || n <= 0 {
				die("--timeout takes milliseconds, a whole number above 0 (got '%s')", argv[i+1])
			}
			o.timeout = argv[i+1]
			i++
		default:
			if strings.HasPrefix(a, "--launcher=") {
				o.launcher, _ = launcherArg(argv, i)
				continue
			}
			if strings.HasPrefix(a, "-") {
				die("resume: unknown option %s (%s)", a, resumeUsage)
			}
			if name != "" {
				die("resume: one agent at a time (got %s and %s)", name, a)
			}
			name = a
		}
	}
	if name == "" {
		die("resume: which agent? (%s)", resumeUsage)
	}
	if o.launcher != "" && o.noLauncher {
		die("resume: --launcher and --no-launcher contradict each other")
	}
	if !nameRE.MatchString(name) {
		die("agent name must be lowercase/digits/-/_ (got '%s')", name)
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

	// ---- checks; nothing is changed, not even a record file, until all pass
	if rec.Kind != "" && rec.Kind != "claude" {
		return refuse("%s is a %s agent; resume knows how to resume claude sessions only", name, rec.Kind)
	}
	if err := identityStill(rec); err != nil {
		return err
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
	if s := freshSession(rec, ags); s != "" {
		rec.Session = s // in memory only, like everything before the start
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
	if d, err := isDirAs(cwd); err != nil {
		return err
	} else if !d {
		return refuse("%s ran in %s, which is gone", name, cwd)
	}
	for _, w := range recWorktrees(rec) {
		ex, err := existsAs(w.Path)
		if err != nil {
			return err
		}
		if !ex {
			return refuse("%s's worktree %s is gone; its work can't be resumed there", name, w.Path)
		}
	}
	// Records from before 0.3.0 don't say how the agent was started (they
	// have no cwd of their own). Don't guess its permissions.
	gated := rec.Gated
	switch {
	case o.gated != "":
		gated = o.gated == "gated"
	case rec.Cwd == "":
		return refuse("%s was dispatched by an older bowery, whose record doesn't say whether it was --gated; "+
			"pass --gated (permission prompts on) or --ungated", name)
	}
	l, err := planLanding(rec, o, ags, ps, all)
	if err != nil {
		return err
	}
	var cur Pane
	if l.placement == "pane" {
		// Its own pane, reused: the one place resume lands among processes it
		// didn't start, so the one to check for another tool's marker.
		if tag, err := handsOff(l.pane); err != nil {
			return err
		} else if tag != "" {
			return refuse("%s holds a process carrying %s, which belongs to another tool; not starting %s there", l.pane, tag, name)
		}
		if cur, err = paneGet(l.pane); err != nil {
			return err
		}
	}

	at, err := checkoutAt(cwd)
	if err != nil {
		return err
	}

	agentArgs := []string{"--"}
	if !gated {
		agentArgs = append(agentArgs, "--dangerously-skip-permissions")
	}
	if rec.Model != "" {
		agentArgs = append(agentArgs, "--model", rec.Model)
	}
	agentArgs = append(agentArgs, "--resume", rec.Session)

	launcher := rec.Launcher
	switch {
	case o.noLauncher:
		launcher = ""
	case o.launcher != "":
		launcher = o.launcher
	case launcher != "":
		// A stored line is typed into a shell too: it gets the same check.
		if why := launcherProblem(launcher); why != "" {
			return refuse("%s's recorded launcher %s; resume with --launcher CMD or --no-launcher", name, why)
		}
	}
	where := l.why
	cd := l.placement == "pane" && norm(cur.Cwd) != norm(cwd)
	if cd {
		where += fmt.Sprintf(" (after cd %s there)", cwd)
	}
	if machine != "" {
		where += " on " + machine
	}
	if rec.As != "" {
		where += " as " + rec.As
	}
	if o.dryRun {
		perm := "permission prompts off"
		if gated {
			perm = "permission prompts on"
		}
		via := ""
		if launcher != "" {
			via = ", through " + launcher
		}
		say("[dry-run] would resume %s (session %s, %s%s) in %s, %s, and send it the resume notice", name, rec.Session, perm, via, cwd, where)
		return nil
	}

	// ---- start
	so := startOpts{placement: l.placement, dir: "right", kind: "claude", timeout: o.timeout, cwd: cwd, launcher: launcher}
	from := ""
	switch l.placement {
	case "pane":
		so.pane = l.pane
		// claude files a session under the directory it starts in.
		if cd {
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
		if l.placement == "pane" {
			undoReusedPane(rec, l.pane, cd, cur.Cwd)
		}
		return err
	}
	p, err := paneGet(pane)
	if err != nil {
		undoStart(m)
		return err
	}

	// The agent is up: record it before anything else can fail, so the
	// record never points at a pane it left.
	rec.PaneID, rec.TerminalID, rec.WorkspaceID, rec.TabID = p.PaneID, p.TerminalID, p.WorkspaceID, p.TabID
	switch l.placement {
	case "swarm":
		rec.Placement, rec.CreatedPane, rec.Owns = "swarm", true, &Owns{Kind: "tab", ID: p.TabID}
	case "workspace":
		rec.Placement, rec.CreatedPane, rec.Owns = "workspace", true, &Owns{Kind: "workspace", ID: p.WorkspaceID}
	}
	// The session stays the record's: claude --resume keeps its id, and
	// only one that matches is confirmation.
	if s := sessionIn(p.PaneID); s != rec.Session {
		warn("couldn't confirm %s's session in %s (herdr reports %q); the record keeps %s", name, p.PaneID, s, rec.Session)
	}
	rec.Cwd, rec.Gated, rec.Launcher = cwd, gated, launcher
	rec.Resumed, rec.Resumes = nowISO(), rec.Resumes+1
	if err := writeRecord(rec); err != nil {
		return fail("%s is running again in %s, but its dispatch record could not be written: %v", name, p.PaneID, err)
	}

	sound := rec.Sound
	if sound == "" {
		sound = "off"
	}
	msg := resumeNotice
	if at != "" {
		msg += " " + at
	}
	if len(o.prompt) > 0 {
		msg += "\n\n" + strings.Join(o.prompt, " ")
	}
	if err := setSound(pane, sound); err != nil {
		warn("%s is running again in %s, but tagging its sound failed: %v (bowery mute|unmute %s)", name, p.PaneID, herdrMsg(err), name)
	}
	if err := herdr(nil, "agent", "prompt", name, msg); err != nil {
		return fail("%s is running again in %s, but the resume notice didn't reach it: %v; "+
			"send it yourself (herdr agent prompt %s \"<notice>\"), or done it", name, p.PaneID, herdrMsg(err), name)
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

// undoReusedPane takes back a failed start in the agent's own reused pane.
// Back at a bare shell, its directory is put back; anything else there (a
// claude that half started) is never typed into: a pane bowery made for the
// agent is closed, and a pane that was someone's own is left, with a warning.
func undoReusedPane(rec *Record, pane string, cd bool, was string) {
	p, err := paneGet(pane)
	blank := false
	if err == nil {
		blank, err = paneBlankShell(p)
	}
	if err != nil {
		// Can't tell what is in it: close nothing, type nothing.
		warn("could not look at %s after the failed start (%v); left as it is, look at it", pane, herdrMsg(err))
		return
	}
	busy := !blank
	if ags, err := agents(); err == nil {
		for _, a := range ags {
			busy = busy || a.PaneID == pane
		}
	}
	switch {
	case !busy:
		if cd && was != "" {
			_ = herdr(nil, "pane", "run", pane, "cd "+shq(was)) // put the shell back where it was
		}
	case rec.CreatedPane:
		if _, err := safeClosePane(pane, false); err != nil {
			warn("could not close %s, which the failed start left something running in: %v", pane, err)
		}
	default:
		warn("the failed start may have left claude running in %s, which isn't bowery's to close; look at it", pane)
	}
}

// identityStill refuses when the record's --as and today's config disagree:
// the identity_command it was dispatched under is gone, or one was added
// that the record has no one for.
func identityStill(rec *Record) error {
	ic, where := cfg.IdentityCommand, cfg.path
	if machine != "" {
		ic = machineConfig(machine).IdentityCommand
	}
	switch {
	case rec.As != "" && ic == "":
		return refuse("%s was dispatched as %s, but there is no identity_command for %s now (%s); "+
			"it would start as the wrong person", rec.Name, rec.As, serverName(), where)
	case rec.As == "" && ic != "" && machine != "":
		return refuse("%s was dispatched with no --as, but %s has identities now (identity_command); "+
			"dispatch it again as someone", rec.Name, serverName())
	}
	return nil
}

func serverName() string {
	if machine != "" {
		return machine
	}
	return "this machine"
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
	if o.placement != "" {
		if c := ownedContainer(rec, ps, all); c != nil {
			return landing{}, refuse("%s's %s %s is still open; resume without --%s to come back there, "+
				"or close it first (a new one would leave it owned by nothing)", rec.Name, c.kind, c.id, o.placement)
		}
		for _, p := range ps {
			if rec.TerminalID != "" && p.TerminalID == rec.TerminalID {
				return landing{}, refuse("%s's pane %s is still open; resume without --%s to come back there, or close it first",
					rec.Name, p.PaneID, o.placement)
			}
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
	if d, err := isDirAs(cwd); err != nil || !d {
		return false, err
	}
	for _, w := range recWorktrees(r) {
		if ex, err := existsAs(w.Path); err != nil || !ex {
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
// process that is gone or can't be read is an error.
func procEnv(pid int) (string, error) {
	// Anything it can't read is an error, not "no marker": refusing is the
	// safe side of a hands-off rule.
	script := fmt.Sprintf(`if [ -r /proc/%[1]d/environ ]; then tr '\0' '\n' < /proc/%[1]d/environ; `+
		// ps -E prints another uid's process as its command alone, exit 0.
		// So the environment is what -E adds after the plain command, and
		// only that suffix is searched: a marker in the arguments is not one.
		`elif c=$(ps -ww -o command= -p %[1]d 2>/dev/null) && e=$(ps -wwE -o command= -p %[1]d 2>/dev/null) `+
		`&& [ "${#e}" -gt "${#c}" ] && [ "${e#"$c"}" != "$e" ]; then printf '%%s\n' "${e#"$c"}"; `+
		`else echo "can't read the environment of pid %[1]d" >&2; exit 3; fi`, pid)
	if machine != "" {
		// As the identity: after `become` the pane's processes are theirs,
		// and only they (or root) can read their environment.
		rc, out, err := sshSh(identityPrefix() + "sh -c " + shq(script))
		if err != nil {
			return "", err
		}
		if rc != 0 {
			return "", refuse("can't read the environment of pid %d on %s (%s), so can't tell whether it carries a hands_off_env marker",
				pid, machine, strings.TrimSpace(out))
		}
		return out, nil
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid)); err == nil {
		return string(b), nil
	}
	cmd := exec.Command("sh", "-c", script)
	if as != "" && cfg.IdentityCommand != "" {
		cmd = exec.Command(expandUser(cfg.IdentityCommand), as, "sh", "-c", script)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", refuse("can't read the environment of pid %d (%s), so can't tell whether it carries a hands_off_env marker",
			pid, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// checkoutAt says where the agent's checkout is now ("Your checkout X is on
// branch B at C."), so it can compare with git log before redoing anything;
// "" when its directory isn't in a git checkout.
func checkoutAt(dir string) (string, error) {
	sha, ok, err := gitOut(dir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	if !ok {
		if strings.Contains(sha, "not a git repository") {
			return "", nil
		}
		// git ran and failed for another reason (an identity wrapper that
		// refused, dubious ownership): say so rather than resume blind.
		return "", fail("can't read the checkout at %s: %s", dir, sha)
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
