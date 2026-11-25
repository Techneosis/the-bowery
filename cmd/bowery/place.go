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
	// Keys are the private key files placements are signed with, by the
	// principal who asks (spec §8.1): `keys = { alice = "~/.ssh/id_ed25519" }`.
	// Without an entry, the invoking user's own placements are signed with
	// ~/.ssh/id_ed25519.
	Keys map[string]string `toml:"keys"`
	// RegionKey is this region's private key, which seals a placement sent
	// to a peer region (spec §6, §8.1). Its public half must be the region
	// file's key.
	RegionKey string `toml:"region_key"`
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
	j.From = reg.Region
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

	cat   *offices.Catalog
	peer  bool            // an office a peer region published to this one
	vocab *offices.Region // the peer region's words, from its catalog
	addr  string          // where its box is reached ("" for this region's)
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

var loadedKeys = map[string]offices.PrivateKey{}

// requesterKey is the asking principal's signing key: [placement] keys,
// or the invoking user's ~/.ssh/id_ed25519. Without one, nothing is
// placed: an office drops what isn't signed by a listed signer.
func requesterKey(who string) offices.PrivateKey {
	if k, ok := loadedKeys["p:"+who]; ok {
		return k
	}
	path := cfg.Placement.Keys[who]
	if path == "" {
		if who != offices.Whoami() && who != os.Getenv("USER") {
			die("no signing key for %s: [placement] keys.%s names the private key it signs placements with", who, who)
		}
		path = "~/.ssh/id_ed25519"
	}
	k, err := offices.LoadPrivateKey(expandUser(path))
	if err != nil {
		die("%s's signing key: %v ([placement] keys)", who, err)
	}
	loadedKeys["p:"+who] = k
	return k
}

// regionKey is this region's private key, checked against the region file.
func regionKey(reg *offices.Region) offices.PrivateKey {
	if k, ok := loadedKeys["region"]; ok {
		return k
	}
	if cfg.Placement.RegionKey == "" {
		die("placing into a peer region needs [placement] region_key: region %s's private key, to seal it", reg.Region)
	}
	k, err := offices.LoadPrivateKey(expandUser(cfg.Placement.RegionKey))
	if err != nil {
		die("region_key: %v", err)
	}
	pin, err := offices.ParsePublicKey(reg.Key)
	if err != nil || !pin.Equal(k.Public) {
		die("region_key (%s) isn't region %s's key, as its region file says it", k.Public.Fingerprint(), reg.Region)
	}
	loadedKeys["region"] = k
	return k
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
	if j.From == "" {
		j.From = reg.Region // a job queued before regions signed
	}
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
	// A peer's offices are the ones its catalogs list: never one it didn't
	// publish to this region (spec §6).
	t, terr := offices.NewTransport(cfg.Placement.Transport, placeLayout, 0)
	if terr != nil {
		die("%v", terr)
	}
	for _, c := range peerCandidates(t, reg, regions, time.Now()) {
		d.Candidates = append(d.Candidates, c)
		if c.Class != "" {
			continue
		}
		switch {
		case len(cfg.Placement.Consider.Offices) > 0 && !contains(cfg.Placement.Consider.Offices, c.Office):
			c.Class, c.Reason = "excluded", "not in consider.offices"
		case len(j.Pin) > 0 && !contains(j.Pin, c.Office):
			c.Class, c.Reason = "excluded", "the job is pinned elsewhere"
		case neverHere(j, c.Region):
			c.Class, c.Reason = "excluded", "never: scope "+j.Scope+" stays in its regions"
		}
	}
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
		if c.peer {
			peerFilter(c, j, time.Now())
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
	// A retry time already past is a stale hint (a peer's catalog, a lagging
	// slot): booked as a retry it would spin whoever runs place --retry.
	// The job retries on the next event or tick instead.
	for _, c := range closed {
		if c.RetryAfter != "" && !offices.Earlier(offices.Stamp(time.Now()), c.RetryAfter) {
			c.RetryAfter = ""
		}
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

// peerCandidates are the offices each considered peer published to this
// region, from catalogs verified against the key this region pins for it.
// A catalog that doesn't verify means no link: its offices are never
// planned over. One that is missing or stale is a wait, not a setup gap.
func peerCandidates(t offices.Transport, reg *offices.Region, regions []string, now time.Time) []*Candidate {
	var out []*Candidate
	for _, name := range regions {
		if name == reg.Region {
			continue
		}
		p := reg.Peers[name]
		if p.Address == "" {
			// "" is this region's own to the transport: a placement would
			// land in one of this region's boxes.
			out = append(out, &Candidate{Office: "*", Region: name, Class: "unreachable",
				Reason: "region " + name + " has no address: nothing can be sent there"})
			continue
		}
		recs, err := t.Catalogs(name, p.Address, reg.Region)
		if err != nil || len(recs) == 0 {
			why := "no catalog from region " + name + " yet"
			if err != nil {
				why = "region " + name + "'s catalogs can't be read: " + err.Error()
			}
			out = append(out, &Candidate{Office: "*", Region: name, Class: "unreachable", Reason: why})
			continue
		}
		for _, rec := range recs {
			doc, err := offices.OpenCatalog(rec.Body, p.Key, name, reg.Region, now)
			switch {
			case errors.Is(err, offices.ErrFuture):
				out = append(out, &Candidate{Office: "*", Machine: rec.Machine, Region: name, Class: "unreachable",
					Reason: strings.TrimPrefix(err.Error(), offices.ErrFuture.Error()+": ") + ": refused until it isn't"})
				continue
			case errors.Is(err, offices.ErrStale):
				out = append(out, &Candidate{Office: "*", Machine: rec.Machine, Region: name, Class: "unreachable",
					Reason: fmt.Sprintf("region %s's catalog for machine %s is stale (valid until %s)", name, rec.Machine, doc.ValidUntil)})
				continue
			case err != nil:
				out = append(out, &Candidate{Office: "*", Machine: rec.Machine, Region: name, Class: "excluded",
					Reason: strings.TrimPrefix(err.Error(), offices.ErrNoLink.Error()+": ") + ": no link"})
				continue
			}
			for _, cat := range doc.Offices {
				out = append(out, &Candidate{Office: cat.Office, Machine: cat.Machine, Region: name, Running: cat.Running,
					Owner: strings.Join(cat.Owner, ","), cat: cat, peer: true, vocab: doc.Vocab.Region(name), addr: p.Address})
			}
		}
	}
	return out
}

// peerFilter is the resolver's filter for a peer's office, from its
// catalog alone: the holdings, the peer region's vocabulary and
// max_expires, the owner's state and its office-wide verdicts. There is no
// probe across regions; the office judges its rules (and its peer rules)
// when the placement arrives.
func peerFilter(c *Candidate, j *offices.Job, now time.Time) {
	cat := c.cat
	why := offices.Mismatch(j, cat, c.vocab)
	if max := c.vocab.MaxExpires; max != "" {
		if exp, err := time.Parse(time.RFC3339, j.Expires); err == nil && exp.Sub(now) > offices.Duration(max, 24*time.Hour) {
			why = append(why, fmt.Sprintf("expires is past region %s's max_expires (%s)", c.Region, max))
		}
	}
	if len(why) > 0 {
		c.Class, c.Reason = "holdings", strings.Join(why, "; ")
		return
	}
	// The owner's state as the peer published it, on the peer's clock: one
	// dated ahead of ours can't be weighed, so the office isn't planned
	// over until it can be (a wait, like a catalog dated in the future).
	if st := cat.State; st != nil && st.At != "" {
		if at, err := time.Parse(time.RFC3339Nano, st.At); err != nil || at.Sub(now) > offices.ClockSkew {
			c.Class, c.Reason = "unreachable", "its owner's state is dated ahead of this clock"
			return
		}
	}
	if st := cat.State; st != nil && st.Applies(j, now) {
		if st.State == "closed" {
			c.Class, c.Reason, c.RetryAfter = "closed", "closed by its owner", st.Until
		}
		return // an owner's open: the office decides the rest
	}
	for _, p := range cat.Verdicts {
		vu, err := time.Parse(time.RFC3339Nano, p.ValidUntil)
		if p.Verdict == "closed" && err == nil && now.Before(vu) {
			c.Class, c.Reason, c.RetryAfter = "closed", p.Reason, p.RetryAfter
			return
		}
	}
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
		// The canonical bytes are fixed here, and signed by the asker; a
		// placement into a peer region is sealed by this region too.
		j.SignedAt = offices.Stamp(time.Now())
		block := j.Encode()
		sig, err := offices.SignWith(requesterKey(j.AskedBy), offices.NSPlacement, block)
		if err != nil {
			die("signing %s as %s: %v", j.ID, j.AskedBy, err)
		}
		stages := map[string]string{"resolve": j.ResolveAt, "resolved": resolvedAt, "sign": j.SignedAt}
		var seal []byte
		if c.Region != reg.Region {
			stages["seal"] = offices.Stamp(time.Now())
			if seal, err = offices.SignWith(regionKey(reg), offices.NSSeal, block); err != nil {
				die("sealing %s for region %s: %v", j.ID, c.Region, err)
			}
		}
		t, err := offices.NewTransport(cfg.Placement.Transport, placeLayout, 0)
		if err != nil {
			die("%v", err)
		}
		stages["send"] = offices.Stamp(time.Now())
		if err := t.Send(c.addr, c.cat.Box, j.ID, offices.SealLetter(block, sig, seal)); err != nil {
			placeLog(map[string]any{"event": "send-failed", "job": j.ID, "office": c.Office, "region": c.Region, "why": err.Error()})
			die("placing %s into %s: %v", j.ID, c.Office, err)
		}
		stages["sent"] = offices.Stamp(time.Now())
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
	for _, c := range filter {
		if c.Class == "unreachable" && !strings.HasPrefix(c.Reason, "on machine") {
			waitingFor += " (" + c.Reason + ")" // the first that might, and why it doesn't
			break
		}
	}
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
	requesterKey(j.AskedBy) // no key, no placement: say so before anything waits
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
	requesterKey(j.AskedBy)
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

// cmdPeers shows each declared peer: the key pinned for it, and what its
// verified catalogs offer this region. A catalog that doesn't verify is
// shown as no link, and its offices aren't listed.
func cmdPeers(argv []string) {
	asJSON := false
	for _, a := range argv {
		if a != "--json" {
			die("peers: unknown option %s", a)
		}
		asJSON = true
	}
	reg := placementSetup()
	t, err := offices.NewTransport(cfg.Placement.Transport, placeLayout, 0)
	if err != nil {
		die("%v", err)
	}
	type row struct {
		Peer       string   `json:"peer"`
		Address    string   `json:"address"`
		Key        string   `json:"key"`
		Considered bool     `json:"considered"`
		MayPlace   []string `json:"may_place"`
		Offices    []string `json:"offices"`
		Problems   []string `json:"problems"`
	}
	names := make([]string, 0, len(reg.Peers))
	for n := range reg.Peers {
		names = append(names, n)
	}
	sort.Strings(names)
	rows := []row{}
	for _, n := range names {
		p := reg.Peers[n]
		r := row{Peer: n, Address: p.Address, Considered: contains(cfg.Placement.Consider.Regions, n),
			MayPlace: append([]string{}, p.MayPlace...), Offices: []string{}, Problems: []string{}}
		if k, err := offices.ParsePublicKey(p.Key); err == nil {
			r.Key = k.Fingerprint()
		} else {
			r.Problems = append(r.Problems, "its pinned key isn't usable: "+err.Error())
		}
		for _, c := range peerCandidates(t, reg, []string{n}, time.Now()) {
			if c.Class != "" {
				r.Problems = append(r.Problems, c.Reason)
			} else {
				r.Offices = append(r.Offices, c.Office)
			}
		}
		rows = append(rows, r)
	}
	if asJSON {
		printLine(rows)
		return
	}
	for _, r := range rows {
		state := "considered"
		if !r.Considered {
			state = "not in consider.regions"
		}
		fmt.Printf("%s  %s  key %s  (%s)\n", r.Peer, r.Address, r.Key, state)
		fmt.Printf("  it may place into: %s\n", strings.Join(r.MayPlace, ", "))
		fmt.Printf("  it publishes to us: %s\n", strings.Join(r.Offices, ", "))
		for _, p := range r.Problems {
			fmt.Printf("  ! %s\n", p)
		}
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
