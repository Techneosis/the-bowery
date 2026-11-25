package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Techneosis/the-bowery/internal/offices"
)

// PlacementConfig is `[placement]` in bowery's config (spec §3.4): which
// regions and offices this bowery plans over and how it ranks them. Bowery
// never sets an office's rules, and an office never reads this.
type PlacementConfig struct {
	Consider struct {
		Regions []string `toml:"regions"`
		Offices []string `toml:"offices"`
	} `toml:"consider"`
	Rank   []string                `toml:"rank"`
	Prefer []string                `toml:"prefer"`
	Never  []NeverRule             `toml:"never"`
	Kinds  map[string]KindDefaults `toml:"kinds"`
	// The placement transport (default "dir"); the offices' machine files
	// name the same one.
	Transport string `toml:"transport"`
	// The office host to ask (default "office" on PATH).
	OfficeCommand string `toml:"office_command"`
}

type NeverRule struct {
	Scope   string   `toml:"scope"`
	Outside []string `toml:"outside"`
}

type KindDefaults struct {
	Surface  string   `toml:"surface"`
	Priority string   `toml:"priority"`
	Expires  string   `toml:"expires"`
	Pin      []string `toml:"pin"`
	Prefer   []string `toml:"prefer"`
}

var defaultRank = []string{"prefer", "holds-repo", "same-machine", "same-region", "fewest-running"}

// Exit codes a dispatcher handles (spec §7.3).
const (
	exitPlaced  = 0
	exitError   = 1
	exitRefused = 3
	exitQueued  = 4
)

// ---------------------------------------------------------------- the job from flags

// jobFlags are the flags that make a job: `bowery place` takes them all;
// `dispatch` takes them too, and any of them makes the dispatch a
// placement.
var jobValued = map[string]bool{"--kind": true, "--job-kind": true, "--office": true, "--scope": true, "--area": true,
	"--needs": true, "--asked-by": true, "--priority": true, "--pin": true, "--tag": true, "--surface": true,
	"--profile": true, "--effects": true, "--credit": true, "--repo": true, "--branch": true, "--base": true,
	"--expires": true, "--id": true, "--name": true, "--worktree": true, "--ticket": true}

// dispatchJobFlags are the ones that turn a dispatch into a placement.
// --kind stays dispatch's agent kind (claude, codex): a job's kind there is
// --job-kind.
var dispatchJobFlags = []string{"--office", "--job-kind", "--scope", "--needs", "--asked-by", "--priority", "--pin", "--tag", "--surface"}

func isPlacementDispatch(argv []string) bool {
	for _, a := range argv {
		if a == "--" {
			return false
		}
		if contains(dispatchJobFlags, a) {
			return true
		}
	}
	return false
}

func newJobID(kind string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	k := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, kind)
	if len(k) > 30 {
		k = k[:30]
	}
	return fmt.Sprintf("%s-%s-%s", k, time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(b))
}

// parseJob builds a job from flags. forDispatch: --kind is the agent's, not
// the job's, and only the dispatch job flags plus --worktree/--base and the
// prompt are taken.
func parseJob(argv []string, forDispatch bool) (*offices.Job, bool) {
	j := &offices.Job{Tags: map[string]string{}}
	jsonOut := true
	var expires string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			j.Prompt = strings.Join(argv[i+1:], " ")
			break
		}
		if a == "--json" {
			continue
		}
		if forDispatch && !contains(dispatchJobFlags, a) && a != "--worktree" && a != "--base" && a != "--ticket" {
			die("dispatch with --office or job flags places the job, and the office's surface starts the agent; %s is for a dispatch bowery starts itself", a)
		}
		if !jobValued[a] || (forDispatch && a == "--kind") {
			die("unknown option %s", a)
		}
		if i+1 >= len(argv) {
			die("%s needs a value", a)
		}
		v := argv[i+1]
		i++
		switch a {
		case "--kind", "--job-kind":
			j.Kind = v
		case "--office", "--pin":
			j.Pin = append(j.Pin, strings.Split(v, ",")...)
		case "--scope":
			j.Scope = v
		case "--area":
			j.Area = v
		case "--asked-by":
			j.AskedBy = v
		case "--priority":
			j.Priority = v
		case "--surface":
			j.Surface = v
		case "--profile":
			j.Profile = v
		case "--effects":
			j.Effects = append(j.Effects, strings.Split(v, ",")...)
		case "--credit":
			j.Credit = v
		case "--repo":
			j.Repo = v
		case "--branch", "--worktree":
			j.Branch = v
		case "--base":
			j.Base = v
		case "--expires":
			expires = v
		case "--id":
			j.ID = v
		case "--name":
			j.Name = v
		case "--ticket":
			j.Tags["ticket"] = v
		case "--tag":
			k, val, ok := strings.Cut(v, "=")
			if !ok || k == "" {
				die("--tag takes key=value (got %q)", v)
			}
			j.Tags[k] = val
		case "--needs":
			k, val, ok := strings.Cut(v, "=")
			if !ok || val == "" {
				die("--needs takes tools=a,b | secrets=… | qualifications=… | repo=owner/name[@mode] (got %q)", v)
			}
			items := strings.Split(val, ",")
			switch k {
			case "tool", "tools":
				j.Needs.Tools = append(j.Needs.Tools, items...)
			case "secret", "secrets":
				j.Needs.Secrets = append(j.Needs.Secrets, items...)
			case "qualification", "qualifications":
				j.Needs.Qualifications = append(j.Needs.Qualifications, items...)
			case "repo":
				r, mode, _ := strings.Cut(val, "@")
				j.Needs.Repo, j.Needs.RepoMode = r, mode
				if j.Repo == "" {
					j.Repo = r
				}
			default:
				die("--needs: %q isn't tools, secrets, qualifications or repo", k)
			}
		}
	}
	if j.Kind == "" {
		if forDispatch {
			die("a placed dispatch needs --job-kind <kind>: offices take jobs by kind")
		}
		die("place needs --kind <kind>")
	}
	if j.AskedBy == "" {
		// The principal who asks: you, by your login name, unless --asked-by.
		if j.AskedBy = os.Getenv("USER"); j.AskedBy == "" {
			j.AskedBy = offices.Whoami()
		}
	}
	if j.ID == "" {
		j.ID = newJobID(j.Kind)
	}
	kd := cfg.Placement.Kinds[j.Kind]
	if j.Surface == "" {
		j.Surface = kd.Surface
	}
	if j.Priority == "" {
		j.Priority = kd.Priority
	}
	if len(j.Pin) == 0 {
		j.Pin = kd.Pin
	}
	if expires == "" {
		expires = kd.Expires
	}
	j.Expires = expires // a duration until the region is read; see finishJob
	return j, jsonOut
}

// finishJob turns the expiry into a time, bounded by the region's
// max_expires, and checks the job's shape.
func finishJob(j *offices.Job, reg *offices.Region) {
	maxExp := offices.Duration(reg.MaxExpires, 24*time.Hour)
	d := maxExp
	if j.Expires != "" {
		var err error
		if d, err = time.ParseDuration(j.Expires); err != nil || d <= 0 {
			die("--expires takes a duration (got %q)", j.Expires)
		}
		if d > maxExp {
			die("--expires %s is past the region's max_expires (%s)", j.Expires, maxExp)
		}
	}
	j.Expires = time.Now().UTC().Add(d).Format(time.RFC3339)
	if j.Priority != "" && len(reg.Priorities) > 0 && !contains(reg.Priorities, j.Priority) {
		die("priority %s isn't one of region %s's (%s)", j.Priority, reg.Region, strings.Join(reg.Priorities, ", "))
	}
	if err := j.Validate(); err != nil {
		die("%v", err)
	}
}

// ---------------------------------------------------------------- the resolver

// Candidate is one row of the filter table: an office, and why it was
// passed over (empty when it passed).
type Candidate struct {
	Office     string `json:"office"`
	Machine    string `json:"machine"`
	Region     string `json:"region"`
	Class      string `json:"class,omitempty"` // excluded, holdings, closed, unreachable
	Reason     string `json:"reason,omitempty"`
	RetryAfter string `json:"retry_after,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Running    int    `json:"running"`

	cat *offices.Catalog
}

type Decision struct {
	Job        *offices.Job `json:"job"`
	Candidates []*Candidate `json:"candidates"`
	Ranking    []string     `json:"ranking"`
	Chosen     *Candidate   `json:"-"`
	Waiting    *Candidate   `json:"-"` // the best office refused for its state or rules
	RetryAfter string       `json:"retry_after,omitempty"`
	Reserved   string       `json:"-"`
}

var placeLayout offices.Layout

func placementSetup() *offices.Region {
	placeLayout = offices.DetectLayout()
	if n := placeLayout.DevNotice(); n != "" {
		warn("%s", n)
	}
	reg, err := offices.LoadRegion(placeLayout)
	if err != nil {
		die("placement needs this machine's region file: %v", err)
	}
	for _, r := range cfg.Placement.Consider.Regions {
		if r != reg.Region {
			if _, ok := reg.Peers[r]; !ok {
				die("%s: [placement] consider.regions names %s, which region %s doesn't declare as a peer", cfg.path, r, reg.Region)
			}
		}
	}
	return reg
}

func officeCmd() string {
	if c := cfg.Placement.OfficeCommand; c != "" {
		return expandUser(c)
	}
	return "office"
}

// askOffice runs the office host, with a deadline: an office that doesn't
// answer is passed over, never waited on.
func askOffice(stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, officeCmd(), args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		for _, l := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
			if !strings.Contains(l, "dev mode:") {
				msg = strings.TrimPrefix(l, "office: ")
			}
		}
		if ctx.Err() != nil {
			msg = "no answer in 15s"
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return out, nil
}

type probeAnswer struct {
	Verdict    string `json:"verdict"`
	Reason     string `json:"reason"`
	RetryAfter string `json:"retry_after"`
}

func resolve(j *offices.Job, reg *offices.Region) *Decision {
	d := &Decision{Job: j, Candidates: []*Candidate{}, Ranking: []string{}}
	j.ResolveAt = offices.Stamp(time.Now())
	for _, e := range j.Effects {
		if contains(reg.ReservedEffects, e) {
			d.Reserved = e
			return d
		}
	}
	here := offices.ThisMachine()
	regions := cfg.Placement.Consider.Regions
	if len(regions) == 0 {
		regions = []string{reg.Region}
	}
	for _, ro := range reg.Offices {
		c := &Candidate{Office: ro.Name, Machine: ro.Machine, Region: reg.Region}
		d.Candidates = append(d.Candidates, c)
		switch {
		case !contains(regions, reg.Region):
			c.Class, c.Reason = "excluded", "its region isn't in consider.regions"
		case len(cfg.Placement.Consider.Offices) > 0 && !contains(cfg.Placement.Consider.Offices, ro.Name):
			c.Class, c.Reason = "excluded", "not in consider.offices"
		case len(j.Pin) > 0 && !contains(j.Pin, ro.Name):
			c.Class, c.Reason = "excluded", "the job is pinned elsewhere"
		case neverHere(j, reg.Region):
			c.Class, c.Reason = "excluded", "never: scope "+j.Scope+" stays in its regions"
		case ro.Machine != here:
			c.Class, c.Reason = "unreachable", "on machine "+ro.Machine+"; this bowery places only into offices on its own machine"
		}
	}
	// Peers' offices arrive with their catalogs; none are planned over yet.
	for _, pin := range j.Pin {
		found := false
		for _, c := range d.Candidates {
			found = found || c.Office == pin
		}
		if !found {
			d.Candidates = append(d.Candidates, &Candidate{Office: pin, Class: "excluded", Reason: "not an office this bowery knows"})
		}
	}
	jb, _ := json.Marshal(j)
	for _, c := range d.Candidates {
		if c.Class != "" {
			continue
		}
		out, err := askOffice(nil, "describe", c.Office, "--json")
		if err != nil {
			c.Class, c.Reason = "unreachable", "doesn't answer: "+err.Error()
			continue
		}
		cat := &offices.Catalog{}
		if err := json.Unmarshal(out, cat); err != nil {
			c.Class, c.Reason = "unreachable", "its describe doesn't decode"
			continue
		}
		c.cat, c.Running, c.Owner = cat, cat.Running, strings.Join(cat.Owner, ",")
		if why := offices.Mismatch(j, cat, reg); len(why) > 0 {
			c.Class, c.Reason = "holdings", strings.Join(why, "; ")
			continue
		}
		// A published office-wide closed verdict needs no probe, unless the
		// owner's override speaks to this job: an open beats a rule, and only
		// the office knows whether the rule is binding. The probe says.
		ownerSpeaks := cat.State != nil && cat.State.Applies(j, time.Now())
		for _, p := range cat.Verdicts {
			if p.Verdict == "closed" && !ownerSpeaks {
				c.Class, c.Reason, c.RetryAfter = "closed", p.Reason, p.RetryAfter
				break
			}
		}
		if c.Class != "" {
			continue
		}
		out, err = askOffice(jb, "describe", c.Office, "--probe")
		if err != nil {
			c.Class, c.Reason = "unreachable", "its probe doesn't answer: "+err.Error()
			continue
		}
		var pa probeAnswer
		if err := json.Unmarshal(out, &pa); err != nil {
			c.Class, c.Reason = "unreachable", "its probe doesn't decode"
			continue
		}
		if pa.Verdict != "open" {
			c.Class, c.Reason, c.RetryAfter = "closed", pa.Reason, pa.RetryAfter
		}
	}
	var pass, closed []*Candidate
	for _, c := range d.Candidates {
		switch c.Class {
		case "":
			pass = append(pass, c)
		case "closed":
			closed = append(closed, c)
		}
	}
	rank(pass, j, reg, here)
	rank(closed, j, reg, here)
	for _, c := range pass {
		d.Ranking = append(d.Ranking, c.Office)
	}
	if len(pass) > 0 {
		d.Chosen = pass[0]
	}
	if len(closed) > 0 {
		d.Waiting = closed[0]
		// The job retries at the earliest office's retry time; an office with
		// none retries on the next event, so the job does too.
		for _, c := range closed {
			if c.RetryAfter == "" {
				d.RetryAfter = ""
				break
			}
			if d.RetryAfter == "" || offices.Earlier(c.RetryAfter, d.RetryAfter) {
				d.RetryAfter = c.RetryAfter
			}
		}
	}
	return d
}

func neverHere(j *offices.Job, region string) bool {
	for _, n := range cfg.Placement.Never {
		if n.Scope == j.Scope && j.Scope != "" && !contains(n.Outside, region) {
			return true
		}
	}
	return false
}

// rank orders offices by the configured criteria, then region and name, so
// the same job lands in the same place. Rules never rank.
func rank(cs []*Candidate, j *offices.Job, reg *offices.Region, here string) {
	crit := cfg.Placement.Rank
	if len(crit) == 0 {
		crit = defaultRank
	}
	prefer := append(append([]string{}, cfg.Placement.Kinds[j.Kind].Prefer...), cfg.Placement.Prefer...)
	score := func(c *Candidate, k string) int {
		switch k {
		case "prefer":
			for i, p := range prefer {
				if p == c.Office || p == "region:"+c.Region {
					return i
				}
			}
			return len(prefer)
		case "holds-repo":
			repo := j.Needs.Repo
			if repo == "" {
				repo = j.Repo
			}
			if c.cat != nil {
				for _, r := range c.cat.Repos {
					if r.Repo == repo && r.House == "local" {
						return 0
					}
				}
			}
			return 1
		case "same-machine":
			if c.Machine == here {
				return 0
			}
			return 1
		case "same-region":
			if c.Region == reg.Region {
				return 0
			}
			return 1
		case "fewest-running":
			return c.Running
		}
		return 0
	}
	sort.SliceStable(cs, func(a, b int) bool {
		for _, k := range crit {
			if sa, sb := score(cs[a], k), score(cs[b], k); sa != sb {
				return sa < sb
			}
		}
		if cs[a].Region != cs[b].Region {
			return cs[a].Region < cs[b].Region
		}
		return cs[a].Office < cs[b].Office
	})
}

// ---------------------------------------------------------------- place, queue, log

func queueDir() string { return filepath.Join(stateDir(), "queue") }

type Queued struct {
	Job        *offices.Job `json:"job"`
	QueuedAt   string       `json:"queued_at"`
	WaitingFor *Candidate   `json:"waiting_for"`
	RetryAfter string       `json:"retry_after,omitempty"`
	Filter     []*Candidate `json:"filter"`
	Told       []string     `json:"told"` // "<who>: <what>", once per state
}

func placeLog(fields map[string]any) {
	fields["time"] = offices.Stamp(time.Now())
	b, _ := json.Marshal(fields)
	f, err := os.OpenFile(filepath.Join(stateDir(), "place.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		die("place.log: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		die("place.log: %v", err)
	}
}

func readQueued(id string) *Queued {
	b, err := os.ReadFile(filepath.Join(queueDir(), id+".json"))
	if err != nil {
		return nil
	}
	q := &Queued{}
	if json.Unmarshal(b, q) != nil {
		return nil
	}
	return q
}

func writeQueued(q *Queued) {
	if err := os.MkdirAll(queueDir(), 0o755); err != nil {
		die("%v", err)
	}
	b, _ := json.MarshalIndent(q, "", "  ")
	if err := offices.WriteAtomic(filepath.Join(queueDir(), q.Job.ID+".json"), append(b, '\n'), 0o644); err != nil {
		die("the queue: %v", err)
	}
}

// tell is the notification integration: where "the asker is told" lands.
// None is configured yet, so it is said on stderr and kept in place.log.
func tell(who, what string) {
	warn("told %s: %s", who, what)
}

// outcome is what one resolver run did, for the JSON line and the exit.
type outcome struct {
	code int
	line map[string]any
}

// placeJob runs the resolver for one job and acts on it: place it, queue
// it, refuse it, or say no office could ever take it.
func placeJob(j *offices.Job, reg *offices.Region) outcome {
	d := resolve(j, reg)
	resolvedAt := offices.Stamp(time.Now())
	filter := d.Candidates
	if d.Reserved != "" {
		placeLog(map[string]any{"event": "refused", "job": j.ID, "kind": j.Kind, "asked_by": j.AskedBy, "reason": "reserved effect " + d.Reserved,
			"told": []string{j.AskedBy, "escalation sink (none configured)"}})
		tell(j.AskedBy, fmt.Sprintf("%s names the reserved effect %s: it is never placed, and goes to a person", j.ID, d.Reserved))
		dropQueued(j.ID)
		return outcome{exitRefused, map[string]any{"refused": map[string]any{"job": j.ID, "reason": "reserved effect " + d.Reserved}}}
	}
	if c := d.Chosen; c != nil {
		j.Office, j.Machine, j.Region = c.Office, c.Machine, c.Region
		j.SignedAt = offices.Stamp(time.Now()) // the canonical bytes are fixed here; signing them lands with regions
		t, err := offices.NewTransport(cfg.Placement.Transport, placeLayout, 0)
		if err != nil {
			die("%v", err)
		}
		j.SentAt = offices.Stamp(time.Now())
		if err := t.Send(c.cat.Box, j.ID, j.Encode()); err != nil {
			placeLog(map[string]any{"event": "send-failed", "job": j.ID, "office": c.Office, "why": err.Error()})
			die("placing %s into %s: %v", j.ID, c.Office, err)
		}
		sentDone := offices.Stamp(time.Now())
		var skipped []map[string]string
		for _, x := range filter {
			if x.Class != "" {
				skipped = append(skipped, map[string]string{"office": x.Office, "reason": x.Reason})
			}
		}
		if skipped == nil {
			skipped = []map[string]string{}
		}
		why := "the only office that passes"
		if len(d.Ranking) > 1 {
			crit := cfg.Placement.Rank
			if len(crit) == 0 {
				crit = defaultRank
			}
			why = fmt.Sprintf("first of %s by %s", strings.Join(d.Ranking, ", "), strings.Join(crit, ", "))
		}
		stages := map[string]string{"resolve": j.ResolveAt, "resolved": resolvedAt, "sign": j.SignedAt, "send": j.SentAt, "sent": sentDone}
		placeLog(map[string]any{"event": "placed", "job": j.ID, "kind": j.Kind, "asked_by": j.AskedBy, "office": c.Office,
			"machine": c.Machine, "region": c.Region, "why": why, "candidates": filter, "ranking": d.Ranking, "stages": stages})
		dropQueued(j.ID)
		return outcome{exitPlaced, map[string]any{"placed": map[string]any{"office": c.Office, "machine": c.Machine,
			"region": c.Region, "id": j.ID, "why": why, "skipped": skipped, "stages": stages}}}
	}
	// Nothing passes. A wait only if some office could take it once open (or
	// once it answers); otherwise it is a setup gap.
	if d.Waiting == nil {
		reachable := false
		for _, c := range filter {
			// An office that holds what the job needs but isn't answering
			// (its describe or its probe) is a wait, not a setup gap.
			reachable = reachable || (c.Class == "unreachable" && !strings.HasPrefix(c.Reason, "on machine"))
		}
		if !reachable {
			gap := "no office holds what " + j.ID + " needs"
			placeLog(map[string]any{"event": "unplaceable", "job": j.ID, "kind": j.Kind, "asked_by": j.AskedBy, "reason": gap, "candidates": filter, "told": []string{j.AskedBy}})
			tell(j.AskedBy, gap+" (a setup gap, not a wait)")
			dropQueued(j.ID)
			return outcome{exitError, map[string]any{"unplaceable": map[string]any{"job": j.ID, "reason": gap, "candidates": filter}}}
		}
	}
	q := readQueued(j.ID)
	if q == nil {
		q = &Queued{Job: j, QueuedAt: offices.Stamp(time.Now()), Told: []string{}}
	}
	q.Job, q.Filter, q.RetryAfter = j, filter, d.RetryAfter
	waitingFor := "no office answers"
	if d.Waiting != nil {
		waitingFor = d.Waiting.Office + ": " + d.Waiting.Reason
	}
	q.WaitingFor = d.Waiting
	var told []string
	note := func(who, what string) {
		key := who + ": " + what
		if !contains(q.Told, key) {
			tell(who, what)
			q.Told = append(q.Told, key)
			if !contains(told, who) {
				told = append(told, who)
			}
		}
	}
	note(j.AskedBy, "waiting for "+waitingFor)
	if d.Waiting != nil && d.Waiting.Owner != "" {
		note(d.Waiting.Owner, fmt.Sprintf("%s waits on %s (%s)", j.ID, d.Waiting.Office, d.Waiting.Reason))
	}
	writeQueued(q)
	if told == nil {
		told = []string{}
	}
	placeLog(map[string]any{"event": "queued", "job": j.ID, "kind": j.Kind, "asked_by": j.AskedBy, "waiting_for": waitingFor,
		"retry_after": d.RetryAfter, "candidates": filter, "told": told,
		"stages": map[string]string{"resolve": j.ResolveAt, "resolved": resolvedAt}})
	return outcome{exitQueued, map[string]any{"queued": map[string]any{"job": j.ID, "waiting_for": waitingFor,
		"retry_after": d.RetryAfter, "told": told}}}
}

func dropQueued(id string) { _ = os.Remove(filepath.Join(queueDir(), id+".json")) }

func printLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		die("%v", err)
	}
	fmt.Println(string(b))
}

// ---------------------------------------------------------------- commands

func cmdPlace(argv []string) {
	if len(argv) > 0 && argv[0] == "--retry" {
		if len(argv) > 1 {
			die("place --retry takes nothing else")
		}
		cmdPlaceRetry()
		return
	}
	j, _ := parseJob(argv, false)
	reg := placementSetup()
	finishJob(j, reg)
	o := placeJob(j, reg)
	printLine(o.line)
	os.Exit(o.code)
}

// placeFromDispatch is `dispatch <name>` with job flags: the job goes to an
// office through the resolver, and the office's surface starts the agent.
func placeFromDispatch(name string, argv []string) {
	if contains(argv, "--machine") {
		die("--machine pins a dispatch bowery starts itself; with job flags use --office to pin it")
	}
	j, _ := parseJob(argv, true)
	j.Name = name
	reg := placementSetup()
	finishJob(j, reg)
	o := placeJob(j, reg)
	printLine(o.line)
	os.Exit(o.code)
}

// cmdPlaceRetry retries every waiting job, in priority order then oldest
// first, and drops the ones past their expiry. Run it on events (an office
// opens, an agent ends) and on a timer: its output's next_retry is the
// earliest retry_after, and the region's retry_tick is the safety net.
func cmdPlaceRetry() {
	reg := placementSetup()
	ents, _ := os.ReadDir(queueDir())
	var qs []*Queued
	for _, e := range ents {
		if q := readQueued(strings.TrimSuffix(e.Name(), ".json")); q != nil && strings.HasSuffix(e.Name(), ".json") {
			qs = append(qs, q)
		}
	}
	sort.SliceStable(qs, func(a, b int) bool {
		ra, rb := offices.PriorityRank(reg, qs[a].Job.Priority), offices.PriorityRank(reg, qs[b].Job.Priority)
		if ra != rb {
			return ra < rb
		}
		return qs[a].QueuedAt < qs[b].QueuedAt
	})
	placed, waiting, dropped := []any{}, []any{}, []any{}
	next := ""
	for _, q := range qs {
		j := q.Job
		if exp, err := time.Parse(time.RFC3339, j.Expires); err != nil || !time.Now().Before(exp) {
			dropQueued(j.ID)
			placeLog(map[string]any{"event": "dropped", "job": j.ID, "reason": "expired while waiting", "told": []string{j.AskedBy}})
			tell(j.AskedBy, j.ID+" expired while waiting; send it again if it is still wanted")
			dropped = append(dropped, map[string]any{"job": j.ID, "reason": "expired while waiting"})
			continue
		}
		o := placeJob(j, reg)
		switch o.code {
		case exitPlaced:
			placed = append(placed, o.line["placed"])
		case exitQueued:
			waiting = append(waiting, o.line["queued"])
			if r := readQueued(j.ID); r != nil && r.RetryAfter != "" && (next == "" || offices.Earlier(r.RetryAfter, next)) {
				next = r.RetryAfter
			}
		default:
			dropped = append(dropped, o.line)
		}
	}
	out := map[string]any{"placed": placed, "waiting": waiting, "dropped": dropped,
		"retry_tick": offices.Duration(reg.Defaults.RetryTick, 10*time.Minute).String()}
	if next != "" {
		out["next_retry"] = next
	}
	printLine(out)
}

func cmdQueue(argv []string) {
	asJSON := false
	for _, a := range argv {
		if a != "--json" {
			die("queue: unknown option %s", a)
		}
		asJSON = true
	}
	ents, _ := os.ReadDir(queueDir())
	list := []*Queued{}
	for _, e := range ents {
		if q := readQueued(strings.TrimSuffix(e.Name(), ".json")); q != nil {
			list = append(list, q)
		}
	}
	if asJSON {
		printLine(list)
		return
	}
	for _, q := range list {
		w := "no office answers"
		if q.WaitingFor != nil {
			w = q.WaitingFor.Office + ": " + q.WaitingFor.Reason
		}
		fmt.Printf("%s  %s  asked by %s  waiting for %s%s\n", q.Job.ID, q.Job.Kind, q.Job.AskedBy, w, optStr(", retry after ", q.RetryAfter))
	}
}

func optStr(p, v string) string {
	if v == "" {
		return ""
	}
	return p + v
}

// cmdOffice is `bowery office ls|describe|log|stop`: the office host's own
// answers, asked on this machine. stop is the owner ending a running or
// held job.
func cmdOffice(argv []string) {
	if len(argv) == 0 {
		die("office: ls, describe <office>, log <office> [--since DUR], stop <office> <job>")
	}
	switch argv[0] {
	case "ls", "describe", "log", "stop":
	default:
		die("office: %s isn't ls, describe, log or stop", argv[0])
	}
	cmd := exec.Command(officeCmd(), argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		die("%s: %v", officeCmd(), err)
	}
}
