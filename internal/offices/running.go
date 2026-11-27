package offices

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Running is one job this office started and hasn't seen end: the record
// the run phase works from. Its placement stays in accepted/, its workspace
// and claimed work stay wherever its surface keeps them, and a held job is
// never re-placed: it resumes here or the owner ends it (spec §4.5).
type Running struct {
	ID       string `json:"id"`
	Office   string `json:"office"`
	Kind     string `json:"kind"`
	Priority string `json:"priority,omitempty"`
	AskedBy  string `json:"asked_by"`
	Scope    string `json:"scope,omitempty"`
	Surface  string `json:"surface"`
	Started  string `json:"started"`
	// State is running, held (asked to yield or finish its turn, or
	// interrupted by the owner's close --now), or stopping (asked to stop
	// at its next turn: the job ends).
	State    string `json:"state"`
	OnClosed string `json:"on_closed,omitempty"`
	Reason   string `json:"reason,omitempty"`
	By       string `json:"by,omitempty"` // the instance that closed, or owner
	HeldAt   string `json:"held_at,omitempty"`
	Yielded  string `json:"yielded,omitempty"` // when the job said it reached its safe point
	Resumed  int    `json:"resumed,omitempty"`
	Pgid     int    `json:"pgid,omitempty"` // the process group its surface started it in
	// Reattached counts the times a pull loop's start gave it a new group
	// through its surface's attach command, after its old one was gone.
	Reattached int `json:"reattached,omitempty"`
}

func (l Layout) RunningDir(o string) string  { return filepath.Join(l.Var, o, "running") }
func (l Layout) RequestsDir(o string) string { return filepath.Join(l.Var, o, "requests") }

func runningFile(l Layout, o, id string) string { return filepath.Join(l.RunningDir(o), id+".json") }

// ReadRunning is the office's running jobs, oldest first. A record that
// doesn't decode is skipped here and reported by the run phase.
func ReadRunning(l Layout, office string) []Running {
	out := []Running{}
	files, _ := filepath.Glob(filepath.Join(l.RunningDir(office), "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		r := Running{}
		if json.Unmarshal(b, &r) != nil || r.ID == "" {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Started < out[k].Started })
	return out
}

// MachineRunning is every office's running jobs on this machine.
func MachineRunning(l Layout) []Running {
	out := []Running{}
	dirs, _ := filepath.Glob(filepath.Join(l.Var, "*", "running"))
	for _, d := range dirs {
		out = append(out, ReadRunning(l, filepath.Base(filepath.Dir(d)))...)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Started < out[k].Started })
	return out
}

func GetRunning(l Layout, office, id string) (*Running, error) {
	if !IDRE.MatchString(id) {
		return nil, fmt.Errorf("%q isn't a job id", id)
	}
	b, err := os.ReadFile(runningFile(l, office, id))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%s isn't running in %s", id, office)
	}
	if err != nil {
		return nil, err
	}
	r := &Running{}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, fmt.Errorf("%s's running record doesn't decode: %v", id, err)
	}
	return r, nil
}

func PutRunning(l Layout, r *Running) error {
	if err := os.MkdirAll(l.RunningDir(r.Office), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	return WriteAtomic(runningFile(l, r.Office, r.ID), append(b, '\n'), 0o644)
}

// lockRunning serialises the running records' read-modify-write: the run
// phase holding or resuming a job, and the job's own `done` and `yielded`.
// It is held for the write only, never across a surface command.
func lockRunning(l Layout, office string) (func(), error) {
	if err := os.MkdirAll(l.VarDir(office), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(l.VarDir(office), "running.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// PutRunningIfPresent writes a running record only if the job's record is
// still there: a pass that judged a job for seconds mustn't write a stale
// copy over a job that ended meanwhile (`office done`), which would bring
// it back held, blocking new work for a job that no longer exists.
func PutRunningIfPresent(l Layout, r *Running) (bool, error) {
	unlock, err := lockRunning(l, r.Office)
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := os.Stat(runningFile(l, r.Office, r.ID)); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, PutRunning(l, r)
}

// UpdateRunning changes a job's record in place, under the lock, from what
// is there now.
func UpdateRunning(l Layout, office, id string, f func(*Running) error) error {
	unlock, err := lockRunning(l, office)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := GetRunning(l, office, id)
	if err != nil {
		return err
	}
	if err := f(r); err != nil {
		return err
	}
	return PutRunning(l, r)
}

// EndRunning removes a job's running record and settles its collapsed
// run-phase log lines.
func EndRunning(l Layout, office, id string) error {
	unlock, err := lockRunning(l, office)
	if err != nil {
		return err
	}
	err = os.Remove(runningFile(l, office, id))
	unlock()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	st := loadStreaks(l, office)
	if err := AppendAll(l, office, st.end(id+"/")); err != nil {
		return err
	}
	return st.save()
}

// HeldReason is why new work waits: the first held job's reason, or "".
func HeldReason(l Layout, office string) string {
	for _, r := range ReadRunning(l, office) {
		if r.State == "held" {
			return fmt.Sprintf("holding %s until the office opens again: %s", r.ID, r.Reason)
		}
	}
	return ""
}

// ---------------------------------------------------------------- requests

// A request is the owner asking the pull job to act on a running job (end
// it). The pull job, which runs as the office, is the only thing that
// talks to the office's agents; the owner only leaves word.
type Request struct {
	Action string `json:"action"` // stop
	Job    string `json:"job"`
	By     string `json:"by"`
	At     string `json:"at"`
}

func PutRequest(l Layout, office string, rq Request) error {
	if !IDRE.MatchString(rq.Job) {
		return fmt.Errorf("%q isn't a job id", rq.Job)
	}
	d := l.RequestsDir(office)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(rq)
	return WriteAtomic(filepath.Join(d, rq.Action+"-"+rq.Job+".json"), b, 0o644)
}

func readRequests(l Layout, office string) ([]Request, []string) {
	files, _ := filepath.Glob(filepath.Join(l.RequestsDir(office), "*.json"))
	sort.Strings(files)
	var out []Request
	var paths []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		rq := Request{}
		if err != nil || json.Unmarshal(b, &rq) != nil {
			_ = os.Remove(f)
			continue
		}
		out = append(out, rq)
		paths = append(paths, f)
	}
	return out, paths
}

// runningEvents is the part of the events fingerprint that running work
// adds: each job's state, and the owner's requests.
func runningEvents(l Layout, office string) string {
	var parts []string
	for _, r := range ReadRunning(l, office) {
		parts = append(parts, r.ID+":"+r.State)
	}
	files, _ := filepath.Glob(filepath.Join(l.RequestsDir(office), "*.json"))
	sort.Strings(files)
	for _, f := range files {
		parts = append(parts, filepath.Base(f))
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------- the run phase

// RunItem is one running job's outcome in one run pass.
type RunItem struct {
	ID       string `json:"id"`
	Action   string `json:"action"` // held, stopping, resumed, stopped, still-held
	OnClosed string `json:"on_closed,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// RunPhase judges every running job with the run-phase rules and the
// owner's state, and acts on what changed (spec §4.5, §5.5):
//
//   - a running job whose result is closed is asked to do what the closing
//     source's on_closed says: held (yield, finish-turn, or now for the
//     owner's close --now), or stopping (stop-next-turn: the job ends);
//   - a held job whose result is open again is re-checked as a resume (the
//     fixed checks except expiry, then the accept-phase rules) and, if that
//     admits it, resumed in place;
//   - the owner's stop requests end jobs.
//
// Held work keeps its record, its placement and whatever its surface holds;
// nothing here re-places it. ev runs the run phase (component run); accept
// re-checks a resume, as the wrapper will once there is one.
func RunPhase(c *Config, ev, accept *Evaluator) ([]RunItem, error) {
	o := c.Office.Name
	items := []RunItem{}
	reqs, paths := readRequests(c.L, o)
	for i, rq := range reqs {
		if rq.Action == "stop" {
			r, err := GetRunning(c.L, o, rq.Job)
			if err == nil {
				code, why := surfaceCmd(c, r, "stop", nil)
				// The owner's stop ends the job's whole process group.
				killGroup(r.Pgid)
				e := Entry{"event": "stopped", "job": r.ID, "by": "owner", "asked_by": rq.By, "was": r.State, "told": why == ""}
				if why != "" {
					e["why"], e["alert"] = why, Alert{"smoke", "couldn't tell " + r.ID + " to stop: " + why}
				} else {
					e["exit"] = code
				}
				if err := Append(c.L, o, e); err != nil {
					return nil, err
				}
				if err := EndRunning(c.L, o, r.ID); err != nil {
					return nil, err
				}
				items = append(items, RunItem{ID: r.ID, Action: "stopped", Reason: "the owner's stop"})
			} else if err := Append(c.L, o, Entry{"event": "request-ignored", "job": rq.Job, "action": rq.Action, "why": err.Error()}); err != nil {
				return nil, err
			}
		}
		_ = os.Remove(paths[i])
	}
	cat := CatalogOf(c)
	st, _ := ReadState(c.L, o)
	for _, r := range ReadRunning(c.L, o) {
		r := r
		if r.State == "stopping" {
			continue // it is ending; it reports done
		}
		j, err := acceptedJob(c, r.ID)
		if err != nil {
			if err := Append(c.L, o, Entry{"event": "run-unreadable", "job": r.ID, "why": err.Error(),
				"alert": Alert{"smoke", "running job " + r.ID + " has no readable placement"}}); err != nil {
				return nil, err
			}
			continue
		}
		res, err := ev.Evaluate("run", j)
		if err != nil {
			return nil, err
		}
		now := ev.Clock().UTC()
		if res.Verdict == "closed" {
			if r.State != "running" {
				continue // already held; it stays held, here
			}
			mode := res.OnClosed
			by := res.By
			if res.By == "rules" && len(res.Instances) > 0 {
				for _, in := range res.Instances {
					if in.Verdict.Verdict == "closed" {
						by = in.Name
						break
					}
				}
			}
			if res.By == "owner" && st != nil && st.Now {
				mode = "now"
			}
			r.OnClosed, r.Reason, r.By, r.HeldAt, r.Yielded = mode, res.Reason, by, Stamp(now), ""
			event := "held"
			r.State = "held"
			if mode == "stop-next-turn" {
				event, r.State = "stopped", "stopping"
			}
			if ok, err := PutRunningIfPresent(c.L, &r); err != nil {
				return nil, err
			} else if !ok {
				continue // it ended while it was being judged
			}
			code, why := surfaceCmd(c, &r, "hold", map[string]string{"OFFICE_ON_CLOSED": mode, "OFFICE_REASON": res.Reason})
			e := Entry{"event": event, "job": r.ID, "on_closed": mode, "reason": res.Reason, "by": by, "told": why == ""}
			if mode == "now" {
				e["owners_stop"] = true
			}
			if why != "" {
				e["why"], e["alert"] = why, Alert{"smoke", "couldn't tell " + r.ID + " to " + mode + ": " + why}
			} else {
				e["exit"] = code
			}
			if err := Append(c.L, o, e); err != nil {
				return nil, err
			}
			items = append(items, RunItem{ID: r.ID, Action: r.State, OnClosed: mode, Reason: res.Reason})
			continue
		}
		if r.State != "held" {
			continue
		}
		// A resume is re-checked: the fixed checks (expiry bounds starting,
		// not running) and the accept-phase rules.
		if j.Office != o || j.Machine != c.Machine.Machine || j.Region != c.Region.Region {
			items = append(items, RunItem{ID: r.ID, Action: "still-held", Reason: "the placement is for another office"})
			continue
		}
		if why := Mismatch(j, cat, c.Region); len(why) > 0 {
			items = append(items, RunItem{ID: r.ID, Action: "still-held", Reason: strings.Join(why, "; ")})
			continue
		}
		acc, err := accept.Evaluate("accept", j)
		if err != nil {
			return nil, err
		}
		if acc.Verdict != "open" {
			items = append(items, RunItem{ID: r.ID, Action: "still-held", Reason: acc.Reason})
			continue
		}
		heldFor := ""
		if ht, err := time.Parse(time.RFC3339Nano, r.HeldAt); err == nil {
			heldFor = now.Sub(ht).Round(time.Millisecond).String()
		}
		was := r.OnClosed
		r.State, r.OnClosed, r.Reason, r.By, r.HeldAt, r.Yielded = "running", "", "", "", "", ""
		r.Resumed++
		if ok, err := PutRunningIfPresent(c.L, &r); err != nil {
			return nil, err
		} else if !ok {
			continue // it ended while it was being judged
		}
		code, why := surfaceCmd(c, &r, "resume", nil)
		e := Entry{"event": "resumed", "job": r.ID, "was": was, "held_for": heldFor, "by": acc.By, "told": why == ""}
		if why != "" {
			e["why"], e["alert"] = why, Alert{"smoke", "couldn't tell " + r.ID + " to resume: " + why}
		} else {
			e["exit"] = code
		}
		if err := Append(c.L, o, e); err != nil {
			return nil, err
		}
		items = append(items, RunItem{ID: r.ID, Action: "resumed"})
	}
	return items, nil
}

// groupGone is whether nothing is left in a process group. A group we may
// not signal (EPERM) still has something in it.
func groupGone(pgid int) bool {
	if pgid <= 1 {
		return false // never known: nothing to say about it
	}
	return syscall.Kill(-pgid, 0) == syscall.ESRCH
}

// Reattach is a pull loop's first act when it starts: every running job
// whose process group is gone (what its surface left running died with the
// loop's last process tree, a service restart that ends its whole cgroup)
// is given a new one through its surface's attach command, and its record
// takes the new group, so the owner's stop and anything reading pgid see
// the job's real processes. A surface without attach is left alone: its
// agent may live outside the group. It holds the pull lock, so two loops
// can't both attach a job.
func Reattach(c *Config) ([]RunItem, error) {
	unlock, err := c.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	o := c.Office.Name
	items := []RunItem{}
	for _, r := range ReadRunning(c.L, o) {
		r := r
		s, ok := c.Machine.Surfaces[r.Surface]
		if !ok || len(s.Attach) == 0 || !groupGone(r.Pgid) {
			continue
		}
		var body []byte
		if j, err := acceptedJob(c, r.ID); err == nil {
			body, _ = json.Marshal(j)
		} else {
			body, _ = json.Marshal(r)
		}
		was := r.Pgid
		code, pgid, err := runSurface(c, s, s.Attach, body, r.ID, r.Surface, map[string]string{"OFFICE_JOB_STATE": r.State})
		if err != nil || code != 0 {
			e := Entry{"event": "attach-failed", "job": r.ID, "surface": r.Surface, "was_pgid": was}
			why := ""
			if err != nil {
				why = err.Error()
				e["why"] = why
			} else {
				e["exit"] = code
				why = fmt.Sprintf("exit %d", code)
			}
			e["alert"] = Alert{"smoke", "couldn't re-attach " + r.ID + " after its process group was gone: " + why}
			if err := Append(c.L, o, e); err != nil {
				return nil, err
			}
			items = append(items, RunItem{ID: r.ID, Action: "attach-failed", Reason: why})
			continue
		}
		n := 0
		if err := UpdateRunning(c.L, o, r.ID, func(cur *Running) error {
			cur.Pgid = pgid
			cur.Reattached++
			n = cur.Reattached
			return nil
		}); err != nil {
			// It ended while it was being attached (its done removed it):
			// what attach started has no job to watch.
			killGroup(pgid)
			continue
		}
		if err := Append(c.L, o, Entry{"event": "reattached", "job": r.ID, "surface": r.Surface, "state": r.State,
			"was_pgid": was, "pgid": pgid, "reattached": n}); err != nil {
			return nil, err
		}
		items = append(items, RunItem{ID: r.ID, Action: "reattached"})
	}
	return items, nil
}

func acceptedJob(c *Config, id string) (*Job, error) {
	b, err := os.ReadFile(filepath.Join(c.L.Accepted(c.Office.Name), id+".placement"))
	if err != nil {
		return nil, err
	}
	return c.ReadLetter(b)
}

// surfaceCmd runs one of a running job's surface commands (hold, resume,
// stop) with the job on stdin. why is "" when the command ran; otherwise it
// says why the job couldn't be told.
func surfaceCmd(c *Config, r *Running, which string, env map[string]string) (int, string) {
	s, ok := c.Machine.Surfaces[r.Surface]
	if !ok {
		return 0, fmt.Sprintf("surface %q isn't on this machine", r.Surface)
	}
	argv := map[string][]string{"hold": s.Hold, "resume": s.Resume, "stop": s.Stop}[which]
	if len(argv) == 0 {
		return 0, fmt.Sprintf("surface %s has no %s command", r.Surface, which)
	}
	j, err := acceptedJob(c, r.ID)
	var body []byte
	if err == nil {
		body, _ = json.Marshal(j)
	} else {
		body, _ = json.Marshal(r)
	}
	code, _, err := runSurface(c, s, argv, body, r.ID, r.Surface, env)
	if err != nil {
		return 0, err.Error()
	}
	return code, ""
}

// runSurface runs a surface command as this process (the office's user in
// root mode), with the job as JSON on stdin, in a process group of its own.
// At the surface's timeout (default a minute) the whole group is killed, so
// nothing it started outlives it unasked. It returns the exit code and the
// group's id, which a job started this way keeps for the owner's stop.
func runSurface(c *Config, s Surface, argv []string, stdin []byte, id, surface string, extra map[string]string) (int, int, error) {
	timeout := Duration(s.Timeout, time.Minute)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Env = surfaceEnv(c, id, surface, extra)
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return 0, 0, err
	}
	pgid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(timeout):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
		return 0, pgid, fmt.Errorf("the surface timed out after %s (its process group was killed)", timeout)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), pgid, nil
	}
	if err != nil {
		return 0, pgid, err
	}
	return 0, pgid, nil
}

// surfaceEnv is a surface's whole environment. In root mode it is built
// from nothing: a fixed PATH, the office user's home, the job's names, and
// nothing of the caller's (no OFFICE_ROOT, no tokens). Dev mode passes the
// caller's PATH and HOME and names its OFFICE_ROOT, as it says it does.
func surfaceEnv(c *Config, id, surface string, extra map[string]string) []string {
	var env []string
	if c.L.Dev {
		env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "OFFICE_ROOT=" + filepath.Dir(c.L.Etc)}
	} else {
		home := "/"
		if u, err := user.Lookup(c.Office.User); err == nil && u.HomeDir != "" {
			home = u.HomeDir
		}
		env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "USER=" + c.Office.User, "LANG=C.UTF-8"}
	}
	env = append(env, "OFFICE_NAME="+c.Office.Name, "OFFICE_JOB_ID="+id, "OFFICE_SURFACE="+surface)
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+extra[k])
	}
	return env
}

// killGroup ends what a job's surface left running in its group: TERM,
// then KILL if anything is still there after two seconds.
func killGroup(pgid int) {
	if pgid <= 1 {
		return
	}
	if syscall.Kill(-pgid, syscall.SIGTERM) != nil {
		return // nothing left in it
	}
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if syscall.Kill(-pgid, 0) != nil {
			return
		}
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// ---------------------------------------------------------------- collapsing

// streaks hold the run phase's repeated verdicts: the first of a run of
// identical verdicts for one job and instance is logged at once; the
// repeats become one line with count, first and last when the run ends (the
// verdict changes, or the job does).
type streak struct {
	Sig   string `json:"sig"`
	Count int    `json:"count"` // repeats since the logged line
	First string `json:"first"`
	Last  string `json:"last"`
	Line  Entry  `json:"line"`
}

type streaks struct {
	path string
	m    map[string]*streak
}

func loadStreaks(l Layout, office string) *streaks {
	s := &streaks{path: filepath.Join(l.VarDir(office), "streaks.json"), m: map[string]*streak{}}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.m)
	}
	return s
}

// see records one verdict. It returns the collapsed line of the run it
// ends (if that run repeated), and whether this verdict starts a new run
// and so is logged itself.
func (s *streaks) see(key, sig string, line Entry, t string) (Entry, bool) {
	cur := s.m[key]
	if cur != nil && cur.Sig == sig {
		if cur.Count == 0 {
			cur.First = t
		}
		cur.Count++
		cur.Last = t
		return nil, false
	}
	var flush Entry
	if cur != nil {
		flush = cur.collapsed()
	}
	s.m[key] = &streak{Sig: sig, Line: line}
	return flush, true
}

// collapsed is the one line for a run's repeats (Count of them, from First
// to Last), after its first verdict, which was logged when it came.
func (st *streak) collapsed() Entry {
	if st.Count < 1 {
		return nil
	}
	e := Entry{}
	for k, v := range st.Line {
		e[k] = v
	}
	e["time"], e["count"], e["first"], e["last"] = st.Last, st.Count, st.First, st.Last
	e["collapsed"] = true
	return e
}

// end settles every run whose key starts with prefix.
func (s *streaks) end(prefix string) []Entry {
	var out []Entry
	keys := make([]string, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if strings.HasPrefix(k, prefix) {
			if e := s.m[k].collapsed(); e != nil {
				out = append(out, e)
			}
			delete(s.m, k)
		}
	}
	return out
}

func (s *streaks) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(s.m)
	return WriteAtomic(s.path, b, 0o644)
}
