package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// resumeNotice is the first thing a resumed agent is told. It may already
// have done part of its job, some of it costly or impossible to undo.
const resumeNotice = "You were resumed: check what you already did before redoing anything. " +
	"Anything that costs money or can't be undone (paid work you placed, a message you sent, a release you published) " +
	"is checked for, not done again."

// freshNotice is the first thing an agent started again in a new
// conversation (--fresh) is told: it remembers nothing of its last one.
const freshNotice = "You were started again in a new conversation; your earlier one isn't loaded. " +
	"Check what's already done (your checkout, your notes, your mail) before redoing anything. " +
	"Anything that costs money or can't be undone is checked for, not done again."

type resumeOpts struct {
	placement, timeout string
	gated              string // "", "gated" or "ungated": overrides what the record says
	launcher           string // overrides the record's launcher
	noLauncher         bool   // start with herdr agent start, dropping the record's launcher
	dryRun             bool
	ifDead             bool          // a no-op (exit 0) when the agent is alive or being resumed
	asJSON             bool          // one JSON line on stdout, whatever happens, with its outcome
	fresh              bool          // start it on a new conversation instead of resuming its session
	notWithin          time.Duration // refuse if it was resumed this recently
	prompt             []string
}

const resumeUsage = "bowery resume <name> [--swarm|--workspace] [--gated|--ungated] [--launcher CMD|--no-launcher] " +
	"[--fresh] [--if-dead] [--not-within DUR] [--json] [--machine LABEL] [--timeout MS] [--dry-run] [-- <prompt>]"

// resumeJSONScan turns on --json's one-line promise for a resume before
// anything can fail (main calls it before loading the config), and finds
// the name: the first word that isn't an option or an option's value.
func resumeJSONScan(argv []string) {
	valued := map[string]bool{"--launcher": true, "--not-within": true, "--timeout": true, "--machine": true}
	seen := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			return
		}
		switch {
		case a == "--json":
			jsonOnce.on = true
		case valued[a]:
			i++
		case strings.HasPrefix(a, "-"):
		case !seen:
			// The word cmdResume takes as the name, valid or not: a refusal
			// of "Foo" names Foo, never a later word or nothing.
			jsonOnce.name, seen = a, true
		}
	}
}

func cmdResume(argv []string) {
	// Everything after a literal -- is the prompt, flags and all.
	var prompt []string
	for i, a := range argv {
		if a == "--" {
			argv, prompt = argv[:i], argv[i+1:]
			break
		}
	}
	resumeJSONScan(argv) // again, in case main didn't
	if jsonOnce.on {
		defer func() {
			if r := recover(); r != nil {
				emitJSON([]kv{{"name", jsonOnce.name}, {"outcome", "failed"}, {"why", fmt.Sprint(r)}})
				fmt.Fprintf(os.Stderr, "bowery: resume: %v\n", r)
				os.Exit(1)
			}
		}()
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
		case "--if-dead":
			o.ifDead = true
		case "--fresh":
			o.fresh = true
		case "--json":
			o.asJSON = true
		case "--not-within":
			if i+1 >= len(argv) {
				die("--not-within needs a duration (10m, 1h)")
			}
			d, err := time.ParseDuration(argv[i+1])
			if err != nil || d <= 0 {
				die("--not-within takes a duration like 10m or 1h (got '%s')", argv[i+1])
			}
			o.notWithin = d
			i++
		case "--timeout":
			if i+1 >= len(argv) {
				die("--timeout needs a value")
			}
			o.timeout = timeoutArg(argv[i+1])
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
	jsonOnce.name = name
	if err := pickMachine(name, explicit); err != nil {
		die("resume: %v", herdrMsg(err))
	}
	err := resumeOne(name, o)
	var rf *Refused
	var sk *skipped
	// --json: a caller reads what happened instead of parsing prose.
	report := func(outcome, why string) {
		if o.asJSON {
			emitJSON([]kv{{"name", name}, {"outcome", outcome}, {"why", why}})
		}
	}
	switch {
	case err == nil:
	case errors.As(err, &sk):
		report(sk.outcome, sk.why)
		if !o.asJSON {
			say("%s: %v; nothing to do", name, sk.why)
		}
	case errors.As(err, &rf):
		report("refused", err.Error())
		die("resume %s: refused, nothing changed: %v", name, err)
	default:
		report("failed", herdrMsg(err))
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
	// One resume or dispatch of a name at a time: two callers (two
	// letters arriving at once) can't both find it dead and start it.
	unlock, held, err := nameLock(name)
	if err != nil {
		return err
	}
	if held {
		if o.ifDead {
			return &skipped{"busy", "another resume or dispatch of it is running"}
		}
		return refuse("another resume or dispatch of %s is running", name)
	}
	defer unlock()
	if r := loadRecord(name); r != nil {
		rec = r // as it is now we hold the lock
	} else {
		return refuse("%s's dispatch record went just now (a done finished first)", name)
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
			if o.ifDead {
				return &skipped{"alive", fmt.Sprintf("it's alive (%s, in %s)", a.AgentStatus, a.PaneID)}
			}
			return refuse("%s is still alive (%s, in %s); nothing to resume", name, a.AgentStatus, a.PaneID)
		}
	}
	if rec.Session == "" && !o.fresh {
		return refuse("%s's record has no session id to resume (an older dispatch never seen alive since, "+
			"or a --fresh start herdr didn't name); bowery resume %s --fresh starts it in a new conversation%s", name, name, oldHerdrNote())
	}
	for _, a := range ags {
		if rec.Session != "" && a.SessionID() == rec.Session {
			if a.Name == "" {
				return refuse("%s's session %s is live in %s, in an unnamed claude, perhaps left by a resume that was taken back; "+
					"close it, or carry on there, first", name, rec.Session, a.PaneID)
			}
			return refuse("%s's session %s is live in %s (%s); one conversation can't run twice", name, rec.Session, a.PaneID, a.Name)
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

	// Last of the checks (everything after it is the start, not a refusal),
	// so that under --if-dead a "too soon" never hides one that needs a person.
	if o.notWithin > 0 && rec.Resumed != "" {
		t, ok := parseStamp(rec.Resumed)
		if !ok {
			return refuse("%s's record says it was resumed at %q, which bowery can't read, so --not-within can't be checked", name, rec.Resumed)
		}
		ago := time.Since(t)
		when := ago.Round(time.Second).String() + " ago"
		if ago < 0 {
			ago, when = 0, "in the future (counted as now)" // a clock that ran ahead
		}
		if ago < o.notWithin {
			why := fmt.Sprintf("it was resumed %s, within --not-within %s", when, o.notWithin)
			if o.ifDead {
				return &skipped{"too_soon", why}
			}
			return refuse("%s", why)
		}
	}

	agentArgs := []string{"--"}
	if !gated {
		agentArgs = append(agentArgs, "--dangerously-skip-permissions")
	}
	if rec.Model != "" {
		agentArgs = append(agentArgs, "--model", rec.Model)
	}
	if !o.fresh {
		agentArgs = append(agentArgs, "--resume", rec.Session)
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
		if o.asJSON {
			emitJSON([]kv{{"name", name}, {"outcome", "dry_run"}, {"fresh", fmt.Sprint(o.fresh)}, {"where", where}})
			return nil
		}
		if o.fresh {
			say("[dry-run] would start %s again in a new conversation (%s%s) in %s, %s, and send it the fresh-start notice", name, perm, via, cwd, where)
		} else {
			say("[dry-run] would resume %s (session %s, %s%s) in %s, %s, and send it the resume notice", name, rec.Session, perm, via, cwd, where)
		}
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
		if l.placement != "pane" {
			undoStart(m)
			return err
		}
		// Its own pane, reused: the agent is up in a pane already known
		// (the same pane and terminal), so record it rather than leave a
		// live agent no record points at.
		warn("couldn't read %s after the start (%v); recording it as the pane it was", pane, herdrMsg(err))
		p = cur
	}

	// The agent is up: record it at once, so a crash from here on (a
	// signal, a dropped ssh) leaves a live seat that is recorded, which a
	// caller sees as alive: the safe side. The record as it was is kept,
	// to put back if this start has to be taken back.
	oldRecord, _ := os.ReadFile(recordPath(name))
	oldSession := rec.Session
	rec.PaneID, rec.TerminalID, rec.WorkspaceID, rec.TabID = p.PaneID, p.TerminalID, p.WorkspaceID, p.TabID
	switch l.placement {
	case "swarm":
		rec.Placement, rec.CreatedPane, rec.Owns = "swarm", true, &Owns{Kind: "tab", ID: p.TabID}
	case "workspace":
		rec.Placement, rec.CreatedPane, rec.Owns = "workspace", true, &Owns{Kind: "workspace", ID: p.WorkspaceID}
	}
	rec.Cwd, rec.Gated, rec.Launcher = cwd, gated, launcher
	rec.Resumed, rec.Resumes = nowISO(), rec.Resumes+1
	var warnings []string
	recorded := true
	if err := writeRecord(rec); err != nil {
		warn("%s is running again in %s, but its dispatch record could not be written: %v", name, p.PaneID, err)
		warnings = append(warnings, "record_not_written")
		recorded = false
	}

	// takeBack undoes a start that mustn't stand, puts the record back as it
	// was, and says only what it knows of what's left.
	takeBack := func() string {
		_ = herdr(nil, "agent", "rename", p.PaneID, "--clear")
		undoStart(m)
		if l.placement == "pane" {
			undoReusedPane(rec, l.pane, cd, cur.Cwd)
		}
		if recorded && len(oldRecord) > 0 {
			if err := restoreRecord(name, oldRecord); err != nil {
				warn("couldn't put %s's record back as it was: %v", name, err)
			}
		}
		ags, err := agents()
		if err != nil {
			return fmt.Sprintf("; it may still be running in %s: check it", p.PaneID)
		}
		for _, a := range ags {
			if a.PaneID == p.PaneID {
				if a.Name == "" {
					return fmt.Sprintf("; it's still running in %s, unnamed: close it or carry on there", p.PaneID)
				}
				return fmt.Sprintf("; it may still be running in %s: check it", p.PaneID)
			}
		}
		if _, err := paneGet(p.PaneID); err == nil && l.placement == "pane" {
			return "; it was stopped, and its pane is left as it was"
		}
		return "; it was taken back"
	}

	// Which conversation came up, within one deadline: on --fresh one that
	// isn't the old; on a resume the record's own. The other one means it's
	// not the start asked for, so it's taken back.
	want := func(s string) bool {
		if o.fresh {
			return s != "" && s != oldSession
		}
		return s == oldSession
	}
	s := ""
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(300 * time.Millisecond) {
		s = sessionNow(p.PaneID)
		// Done when it's the one asked for. On a resume, another named one
		// is an answer too. On --fresh the old one may be a stale report,
		// so keep looking until the deadline.
		if want(s) || (!o.fresh && s != "") || time.Now().After(deadline) {
			break
		}
	}
	switch {
	case want(s):
		if o.fresh && rec.Session != s {
			rec.Session = s
			if recorded {
				if err := writeRecord(rec); err != nil {
					warn("couldn't record %s's new session: %v", name, err)
				}
			}
		}
	case s == "":
		warn("couldn't confirm %s's session in %s; the record keeps %q until a list or watch sees it%s", name, p.PaneID, rec.Session, oldHerdrNote())
		warnings = append(warnings, "session_unconfirmed")
	case o.fresh:
		left := takeBack()
		return fail("%s came back on its old conversation %s, not a new one (does the launcher pass --resume itself?)%s", name, oldSession, left)
	default:
		left := takeBack()
		return fail("%s came up on conversation %s, not its own %s (does the launcher pass its arguments on?)%s", name, s, oldSession, left)
	}

	sound := rec.Sound
	if sound == "" {
		sound = "off"
	}
	msg := resumeNotice
	if o.fresh {
		msg = freshNotice
	}
	if at != "" {
		msg += " " + at
	}
	if len(o.prompt) > 0 {
		msg += "\n\n" + strings.Join(o.prompt, " ")
	}
	if err := setSound(pane, sound); err != nil {
		warn("%s is running again in %s, but tagging its sound failed: %v (bowery mute|unmute %s)", name, p.PaneID, herdrMsg(err), name)
	}
	// The first prompt is why it's up (the notice, and the caller's reason):
	// one that never arrives leaves a seat sitting idle, so it's taken back,
	// its record put back, and reported failed, for the caller to try again.
	if err := herdr(nil, "agent", "prompt", name, msg); err != nil {
		left := takeBack()
		return fail("%s started, but its first prompt didn't reach it: %v%s", name, herdrMsg(err), left)
	}

	started := "resumed"
	if o.fresh {
		started = "started_fresh"
	}
	out := []kv{{"name", name}, {"outcome", started}, {"pane_id", p.PaneID}, {"workspace_id", p.WorkspaceID}, {"tab_id", p.TabID},
		{"sound", sound}, {"session", rec.Session}, {"resumes", fmt.Sprint(rec.Resumes)}}
	if machine != "" {
		out = append(out, kv{"machine", machine})
	}
	if rec.As != "" {
		out = append(out, kv{"as", rec.As})
	}
	if len(warnings) > 0 {
		out = append(out, kv{"warning", strings.Join(warnings, ",")})
	}
	if !recorded && !o.asJSON {
		fmt.Println(jsonLine(out))
		return fail("%s is running again in %s, but its dispatch record could not be written", name, p.PaneID)
	}
	emitJSON(out)
	return nil
}

// sessionNow is the session herdr reports for the agent in a pane, or ""
// (one read, no retries: callers keep their own deadline).
func sessionNow(pane string) string {
	ags, err := agents()
	if err != nil {
		return ""
	}
	for _, a := range ags {
		if a.PaneID == pane {
			return a.SessionID()
		}
	}
	return ""
}

// restoreRecord puts a record file back exactly as it was read.
func restoreRecord(name string, b []byte) error {
	path := recordPath(name)
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o644)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	return os.Rename(tmp, path)
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

// skipped is --if-dead's "nothing to do": not an error, not a refusal.
type skipped struct{ outcome, why string } // outcome: alive, busy or too_soon

func (s *skipped) Error() string { return s.why }

// parseStamp reads a record's time, in the forms bowery has written.
func parseStamp(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02T15:04:05-07:00", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// nameLock takes the lock on a name, per server, without waiting: resume
// and dispatch hold it while they check and change that agent (done
// doesn't yet). held
// is true when another of them has it. Lock files live in the state dir's
// locks/ folder and stay there (removing one would race a waiter).
func nameLock(name string) (unlock func(), held bool, err error) {
	dir := filepath.Join(stateDir(), "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, fail("lock on %s: making %s: %v", name, dir, err)
	}
	p := filepath.Join(dir, name+"."+serverTag()+".lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, false, fail("lock on %s: %v", name, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, fail("lock on %s: %v", name, err)
	}
	return func() { f.Close() }, false, nil
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
					if a.Name == "" && (a.Kind == "" || a.Kind == "claude") {
						return landing{}, refuse("%s's pane %s holds an unnamed claude (left by a resume that was taken back, "+
							"or started by hand); close it, or carry on there, first", rec.Name, p.PaneID)
					}
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
	c, err := ownedContainer(rec, ps, all)
	if err != nil {
		return landing{}, err
	}
	if o.placement != "" {
		if c != nil {
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
		if c != nil && len(c.panes) > 0 {
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
// resumeCmd is the command that would bring a dead record's agent back:
// --fresh when there's no session to resume, and --gated|--ungated when the
// record is too old to say how it was started.
func resumeCmd(r *Record) string {
	c := "bowery resume " + r.Name
	if r.Session == "" {
		c += " --fresh"
	}
	if r.Cwd == "" {
		c += " --gated|--ungated"
	}
	return c
}

// resumable says whether resume (perhaps --fresh: see resumeCmd) could bring
// a dead record's agent back: a claude agent whose directory and worktrees
// are still there.
func resumable(r *Record) (bool, error) {
	if r.Kind != "" && r.Kind != "claude" {
		return false, nil
	}
	cwd := r.Cwd
	if cwd == "" {
		cwd = r.Worktree
	}
	if cwd == "" {
		return false, nil
	}
	// As resume would look: as the agent's identity, which may see what
	// the caller of list or orphans can't (or not see what it can).
	defer func(was string) { as = was }(as)
	as = r.As
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
