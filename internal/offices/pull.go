package offices

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// PullItem is one placement's outcome in one pass of the pull job.
type PullItem struct {
	ID         string `json:"id"`
	Reason     string `json:"reason,omitempty"`
	RetryAfter string `json:"retry_after,omitempty"`
	Surface    string `json:"surface,omitempty"`
	Exit       *int   `json:"exit,omitempty"`
}

type PullReport struct {
	Office    string     `json:"office"`
	Run       []RunItem  `json:"run"`
	Accepted  []PullItem `json:"accepted"`
	Waiting   []PullItem `json:"waiting"`
	Refused   []PullItem `json:"refused"`
	Dropped   []PullItem `json:"dropped"`
	NextRetry string     `json:"next_retry,omitempty"`
}

// pending is what the pull job remembers about a placement it keeps
// waiting: when it arrived, and when to look again.
type pending struct {
	DeliveredAt string `json:"delivered_at"`
	LastEval    string `json:"last_eval"`
	RetryAfter  string `json:"retry_after,omitempty"`
	Reason      string `json:"reason"`
	Events      string `json:"events"` // the mtimes of the state and config when it was last judged
}

func (c *Config) pendingFile() string {
	return filepath.Join(c.L.VarDir(c.Office.Name), "pending.json")
}

func (c *Config) readPending() map[string]pending {
	m := map[string]pending{}
	if b, err := os.ReadFile(c.pendingFile()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// events is a fingerprint of what can change a waiting job's verdict
// without a retry time: the owner's state, the config files, the machine's
// state-file gates, and the office's running work and the owner's requests.
func (c *Config) Events() string {
	var parts []string
	files := []string{c.L.StateFile(c.Office.Name), c.L.RegionFile(), c.L.MachineFile(), c.L.OfficeFile(c.Office.Name)}
	names := make([]string, 0, len(c.Machine.Plugins))
	for n, p := range c.Machine.Plugins {
		if p.Kind == "state-file" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		files = append(files, c.Machine.Plugins[n].Path)
	}
	for _, p := range files {
		if fi, err := os.Stat(p); err == nil {
			parts = append(parts, fmt.Sprint(fi.ModTime().UnixNano()))
		} else {
			parts = append(parts, "-")
		}
	}
	parts = append(parts, runningEvents(c.L, c.Office.Name))
	return strings.Join(parts, ",")
}

// stateExpiry logs, once, that an owner's override with an until has run
// out (spec §11 phase 4: close --until expires on time), and returns the
// until if it is still ahead, so the loop can wake for it.
func (c *Config) stateExpiry(now time.Time) (string, error) {
	st, err := ReadState(c.L, c.Office.Name)
	if err != nil || st == nil || st.Until == "" {
		return "", nil
	}
	u, err := time.Parse(time.RFC3339, st.Until)
	if err != nil {
		return "", nil
	}
	if now.Before(u) {
		return st.Until, nil
	}
	mark := filepath.Join(c.L.VarDir(c.Office.Name), "state-expired")
	key := st.State + " " + st.At + " " + st.Until
	if b, _ := os.ReadFile(mark); string(b) == key {
		return "", nil
	}
	if err := Append(c.L, c.Office.Name, Entry{"event": "state-expired", "state": st.State, "by": st.By, "at": st.At,
		"until": st.Until}); err != nil {
		return "", err
	}
	return "", WriteAtomic(mark, []byte(key), 0o644)
}

// Lock holds the office's pull lock (one pull at a time) until the returned
// func runs.
func (c *Config) Lock() (func(), error) {
	if err := os.MkdirAll(c.L.VarDir(c.Office.Name), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(c.L.VarDir(c.Office.Name), "pull.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// Pull runs one pass of the office's pull job over its inbox: every fixed
// check of spec §8.2 that M1 has, then the accept-phase rules and the
// owner's state, accepting, refusing, dropping or keeping each placement
// waiting. force re-judges every waiting placement; without it (the loop) a
// waiting one is judged again only once its retry_after passes, the state
// or config changes, or the region's retry_tick has gone by since.
func Pull(c *Config, ev *Evaluator, t Transport, in Inbox, force bool) (*PullReport, error) {
	unlock, err := c.Lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := c.stateExpiry(ev.Clock()); err != nil {
		return nil, err
	}
	// Running work first: hold what a closing source asks to hold, resume
	// what may resume, end what the owner stopped. Then new work.
	rev := NewEvaluator(c, "run")
	rev.Clock, rev.Facts = ev.Clock, ev.Facts
	run, err := RunPhase(c, rev, ev)
	if err != nil {
		return nil, err
	}
	letters, err := in.List()
	if err != nil {
		return nil, err
	}
	rep := &PullReport{Office: c.Office.Name, Run: run, Accepted: []PullItem{}, Waiting: []PullItem{}, Refused: []PullItem{}, Dropped: []PullItem{}}
	pend := c.readPending()
	o := c.Office.Name
	type item struct {
		l Letter
		j *Job
	}
	var items []item
	for _, l := range letters {
		j, derr := Decode(l.Body)
		now := time.Now()
		if derr != nil || j.ID != l.ID {
			why := "not a placement this office can read"
			if derr == nil {
				why = "its id doesn't match its letter"
			}
			// Dropped: replies to nobody, and never echoes what it said.
			if err := Append(c.L, o, Entry{"event": "drop", "letter": l.ID, "why": why}); err != nil {
				return nil, err
			}
			_ = in.Done(l, c.L.Refused(o))
			delete(pend, l.ID)
			rep.Dropped = append(rep.Dropped, PullItem{ID: l.ID, Reason: why})
			continue
		}
		if _, seen := pend[j.ID]; !seen {
			p := pending{DeliveredAt: Stamp(now)}
			pend[j.ID] = p
			if err := Append(c.L, o, Entry{"event": "delivered", "job": j.ID, "kind": j.Kind, "asked_by": j.AskedBy,
				"stages": stages(j, p.DeliveredAt, "", "")}); err != nil {
				return nil, err
			}
		}
		items = append(items, item{l, j})
	}
	// Priority order, then oldest first.
	sort.SliceStable(items, func(a, b int) bool {
		ra, rb := PriorityRank(c.Region, items[a].j.Priority), PriorityRank(c.Region, items[b].j.Priority)
		if ra != rb {
			return ra < rb
		}
		return items[a].j.SignedAt < items[b].j.SignedAt
	})
	events := c.Events()
	tick := Duration(c.Region.Defaults.RetryTick, 10*time.Minute)
	cat := CatalogOf(c)
	for _, it := range items {
		j, l := it.j, it.l
		p := pend[j.ID]
		now := time.Now().UTC()
		refuse := func(why string) error {
			if err := Append(c.L, o, Entry{"event": "place-refused", "job": j.ID, "kind": j.Kind, "scope": j.Scope,
				"asked_by": j.AskedBy, "region": j.Region, "reason": why}); err != nil {
				return err
			}
			_ = in.Done(l, c.L.Refused(o))
			delete(pend, j.ID)
			rep.Refused = append(rep.Refused, PullItem{ID: j.ID, Reason: why})
			// Once, to the asker, with the reason.
			if err := t.Reply(j.AskedBy, j.ID, []byte(fmt.Sprintf("refused %s by %s: %s\n", j.ID, o, why))); err != nil {
				return Append(c.L, o, Entry{"event": "reply-failed", "job": j.ID, "why": err.Error()})
			}
			return nil
		}
		// Bound to this office: a copy delivered anywhere else fails.
		if j.Office != o || j.Machine != c.Machine.Machine || j.Region != c.Region.Region {
			if err := refuse("the placement is for another office"); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := os.Stat(filepath.Join(c.L.Accepted(o), j.ID+".placement")); err == nil {
			// One per placement id: a replay maps to the one already made,
			// and its asker hears so once (refused and expired ids aren't
			// here: they are judged again).
			replied := false
			if err := os.MkdirAll(c.L.Replied(o), 0o755); err == nil {
				if f, err := os.OpenFile(filepath.Join(c.L.Replied(o), j.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err == nil {
					f.Close()
					replied = true
					if err := t.Reply(j.AskedBy, j.ID, []byte(fmt.Sprintf("already accepted %s at %s; a replay starts nothing\n", j.ID, o))); err != nil {
						if err := Append(c.L, o, Entry{"event": "reply-failed", "job": j.ID, "why": err.Error()}); err != nil {
							return nil, err
						}
					}
				}
			}
			if err := Append(c.L, o, Entry{"event": "place-duplicate", "job": j.ID, "replied": replied}); err != nil {
				return nil, err
			}
			_ = in.Done(l, "")
			delete(pend, j.ID)
			continue
		}
		if exp, err := time.Parse(time.RFC3339Nano, j.Expires); err != nil || !now.Before(exp) {
			if err := Append(c.L, o, Entry{"event": "place-expired", "job": j.ID, "asked_by": j.AskedBy, "expires": j.Expires}); err != nil {
				return nil, err
			}
			_ = in.Done(l, c.L.Expired(o))
			delete(pend, j.ID)
			_ = t.Reply(j.AskedBy, j.ID, []byte(fmt.Sprintf("expired %s at %s while waiting; send it again if it is still wanted\n", j.ID, o)))
			rep.Dropped = append(rep.Dropped, PullItem{ID: j.ID, Reason: "expired while waiting"})
			continue
		}
		if why := Mismatch(j, cat, c.Region); len(why) > 0 {
			if err := refuse(strings.Join(why, "; ")); err != nil {
				return nil, err
			}
			continue
		}
		if max := c.Region.MaxExpires; max != "" {
			// Counted from the signing time. A placement with none can't be
			// bounded, so it is refused rather than let through.
			sa, serr := time.Parse(time.RFC3339Nano, j.SignedAt)
			exp, _ := time.Parse(time.RFC3339Nano, j.Expires)
			if serr != nil || exp.Sub(sa) > Duration(max, 24*time.Hour) {
				if err := refuse("expires is past the region's max_expires, or the placement has no signing time"); err != nil {
					return nil, err
				}
				continue
			}
		}
		// While the office holds running work, new work pauses (spec §4.5).
		if held := HeldReason(c.L, o); held != "" {
			if p.Reason != held {
				if err := Append(c.L, o, Entry{"event": "place-waiting", "job": j.ID, "kind": j.Kind, "scope": j.Scope,
					"asked_by": j.AskedBy, "region": j.Region, "reason": held}); err != nil {
					return nil, err
				}
			}
			p.Reason, p.RetryAfter, p.LastEval, p.Events = held, "", "", ""
			pend[j.ID] = p
			rep.Waiting = append(rep.Waiting, PullItem{ID: j.ID, Reason: held})
			continue
		}
		// A waiting placement is judged again only on an event or its time.
		if !force && p.LastEval != "" {
			due := false
			if p.Events != events {
				due = true
			}
			if p.RetryAfter != "" {
				if ra, err := time.Parse(time.RFC3339Nano, p.RetryAfter); err == nil && !now.Before(ra) {
					due = true
				}
			}
			if le, err := time.Parse(time.RFC3339Nano, p.LastEval); err == nil && now.Sub(le) >= tick {
				due = true
			}
			if !due {
				rep.Waiting = append(rep.Waiting, PullItem{ID: j.ID, Reason: p.Reason, RetryAfter: p.RetryAfter})
				continue
			}
		}
		res, err := ev.Evaluate("accept", j)
		if err != nil {
			return nil, err
		}
		if res.Verdict != "open" {
			if p.LastEval == "" || p.Reason != res.Reason {
				if err := Append(c.L, o, Entry{"event": "place-waiting", "job": j.ID, "kind": j.Kind, "scope": j.Scope,
					"asked_by": j.AskedBy, "region": j.Region, "reason": res.Reason, "retry_after": res.RetryAfter}); err != nil {
					return nil, err
				}
			}
			p.LastEval, p.Reason, p.RetryAfter, p.Events = Stamp(now), res.Reason, res.RetryAfter, events
			pend[j.ID] = p
			rep.Waiting = append(rep.Waiting, PullItem{ID: j.ID, Reason: res.Reason, RetryAfter: res.RetryAfter})
			continue
		}
		if err := in.Done(l, c.L.Accepted(o)); err != nil {
			return nil, fmt.Errorf("accepting %s: %v", j.ID, err)
		}
		acceptedAt := Stamp(time.Now())
		delete(pend, j.ID)
		if err := Append(c.L, o, Entry{"event": "place-accepted", "job": j.ID, "kind": j.Kind, "scope": j.Scope,
			"asked_by": j.AskedBy, "region": j.Region, "by": res.By, "stages": stages(j, p.DeliveredAt, acceptedAt, "")}); err != nil {
			return nil, err
		}
		surface := launchSurface(c, j)
		item := PullItem{ID: j.ID, Surface: surface}
		code, pgid, lerr := launch(c, j)
		startedAt := Stamp(time.Now())
		if lerr != nil {
			if err := Append(c.L, o, Entry{"event": "launch-failed", "job": j.ID, "surface": surface, "why": lerr.Error()}); err != nil {
				return nil, err
			}
			item.Reason = lerr.Error()
		} else {
			item.Exit = &code
			if err := Append(c.L, o, Entry{"event": "launched", "job": j.ID, "surface": surface, "exit": code,
				"stages": stages(j, p.DeliveredAt, acceptedAt, startedAt)}); err != nil {
				return nil, err
			}
			// A surface that started its agent (exit 0) leaves a running job,
			// until the job reports done or the owner stops it.
			if code == 0 {
				r := &Running{ID: j.ID, Office: o, Kind: j.Kind, Priority: j.Priority, AskedBy: j.AskedBy, Scope: j.Scope,
					Surface: surface, Started: startedAt, State: "running", Pgid: pgid}
				if err := PutRunning(c.L, r); err != nil {
					return nil, err
				}
			}
		}
		rep.Accepted = append(rep.Accepted, item)
	}
	// Forget placements no longer in the box.
	inBox := map[string]bool{}
	for _, it := range items {
		inBox[it.j.ID] = true
	}
	for id := range pend {
		if !inBox[id] {
			delete(pend, id)
		}
	}
	for _, w := range rep.Waiting {
		if w.RetryAfter != "" && (rep.NextRetry == "" || later(rep.NextRetry, w.RetryAfter)) {
			rep.NextRetry = w.RetryAfter
		}
	}
	b, _ := json.Marshal(pend)
	if err := WriteAtomic(c.pendingFile(), b, 0o644); err != nil {
		return nil, err
	}
	return rep, nil
}

// stages is the latency record: the time of each placement stage that has
// happened yet. "sign" is when the block was made (placements are unsigned
// until signatures land; it is still the moment the bytes were fixed).
func stages(j *Job, delivered, accepted, started string) map[string]string {
	m := map[string]string{}
	for k, v := range map[string]string{"resolve": j.ResolveAt, "sign": j.SignedAt, "send": j.SentAt,
		"delivered": delivered, "accepted": accepted, "started": started} {
		if v != "" {
			m[k] = v
		}
	}
	return m
}

// launch starts an accepted job through the office's surface: the machine
// file's command for it, run as this process (the office's user, in root
// mode), with the job as JSON on stdin. A minute to start; the agent itself
// may run on.
// launchSurface is the surface a job starts through: its own, or the
// office's first.
func launchSurface(c *Config, j *Job) string {
	surface := SurfaceName(j.Surface)
	if surface == "" && len(c.Office.Surfaces) > 0 {
		surface = c.Office.Surfaces[0]
	}
	return surface
}

func launch(c *Config, j *Job) (int, int, error) {
	surface := launchSurface(c, j)
	s, ok := c.Machine.Surfaces[surface]
	if !ok || len(s.Command) == 0 {
		return 0, 0, fmt.Errorf("surface %q has no command on this machine", surface)
	}
	b, _ := json.Marshal(j)
	return runSurface(c, s, s.Command, b, j.ID, surface, nil)
}

// NextDue is when the pull loop must look again even with no mail or
// event: the earliest of a waiting placement's retry, an owner's until, and
// (with running work) the run-phase interval. Zero means only the backstop.
func NextDue(c *Config, rep *PullReport, now time.Time) time.Time {
	var due time.Time
	consider := func(t time.Time) {
		if !t.IsZero() && (due.IsZero() || t.Before(due)) {
			due = t
		}
	}
	if rep.NextRetry != "" {
		if t, err := time.Parse(time.RFC3339Nano, rep.NextRetry); err == nil {
			consider(t)
		}
	}
	if u, _ := c.stateExpiry(now); u != "" {
		if t, err := time.Parse(time.RFC3339, u); err == nil {
			consider(t)
		}
	}
	if len(ReadRunning(c.L, c.Office.Name)) > 0 {
		consider(now.Add(c.RunInterval()))
	}
	return due
}
