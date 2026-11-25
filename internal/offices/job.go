package offices

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Needs is what a job needs an office to hold (spec §7.1).
type Needs struct {
	Tools          []string `json:"tools"`
	Secrets        []string `json:"secrets"`
	Qualifications []string `json:"qualifications"`
	Repo           string   `json:"repo,omitempty"`
	RepoMode       string   `json:"repo_mode,omitempty"`
}

// Job is one request to start one agent. Office, Machine and Region bind a
// placement to the office it was sent to; they are empty in a job not yet
// placed.
type Job struct {
	ID       string            `json:"id"`
	Name     string            `json:"name,omitempty"`
	Kind     string            `json:"kind"`
	AskedBy  string            `json:"asked_by"`
	Scope    string            `json:"scope,omitempty"`
	Area     string            `json:"area,omitempty"`
	Priority string            `json:"priority,omitempty"`
	Needs    Needs             `json:"needs"`
	Effects  []string          `json:"effects"`
	Credit   string            `json:"credit,omitempty"`
	Surface  string            `json:"surface,omitempty"`
	Profile  string            `json:"profile,omitempty"`
	Repo     string            `json:"repo,omitempty"`
	Branch   string            `json:"branch,omitempty"`
	Base     string            `json:"base,omitempty"`
	Tags     map[string]string `json:"tags"`
	Pin      []string          `json:"pin,omitempty"`
	Expires  string            `json:"expires"`
	SignedAt string            `json:"signed_at,omitempty"`
	Prompt   string            `json:"prompt,omitempty"`

	Office  string `json:"office,omitempty"`
	Machine string `json:"machine,omitempty"`
	Region  string `json:"region,omitempty"`

	// Stage times, for the latency record: when the resolver started on it,
	// and when the placement was written to the office's box.
	ResolveAt string `json:"resolve_at,omitempty"`
	SentAt    string `json:"sent_at,omitempty"`
}

var (
	IDRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)
	LabelRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,127}$`)
)

// Validate checks the shape of every field, so nothing odd reaches a rule,
// a log line or a placement block.
func (j *Job) Validate() error {
	if !IDRE.MatchString(j.ID) {
		return fmt.Errorf("job id %q: letters, digits, . _ -, up to 80", j.ID)
	}
	if j.Kind == "" {
		return fmt.Errorf("a job needs a kind")
	}
	if j.AskedBy == "" {
		return fmt.Errorf("a job needs asked_by")
	}
	if strings.Contains(j.AskedBy, "/") {
		return fmt.Errorf("asked_by %q: a principal's name has no slash", j.AskedBy)
	}
	single := map[string]string{"kind": j.Kind, "asked_by": j.AskedBy, "scope": j.Scope, "area": j.Area,
		"priority": j.Priority, "surface": j.Surface, "profile": j.Profile, "repo": j.Repo, "branch": j.Branch,
		"base": j.Base, "office": j.Office, "machine": j.Machine, "region": j.Region, "name": j.Name,
		"needs.repo": j.Needs.Repo, "needs.repo_mode": j.Needs.RepoMode}
	for k, v := range single {
		if v != "" && !LabelRE.MatchString(v) {
			return fmt.Errorf("%s %q: letters, digits and . _ : / @ + -", k, v)
		}
	}
	lists := map[string][]string{"needs.tools": j.Needs.Tools, "needs.secrets": j.Needs.Secrets,
		"needs.qualifications": j.Needs.Qualifications, "effects": j.Effects, "pin": j.Pin}
	for k, xs := range lists {
		for _, v := range xs {
			if !LabelRE.MatchString(v) {
				return fmt.Errorf("%s %q: letters, digits and . _ : / @ + -", k, v)
			}
		}
	}
	for k, v := range j.Tags {
		if !LabelRE.MatchString(k) || strings.ContainsAny(v, "\x00\n\r") {
			return fmt.Errorf("tag %q: a label key and a one-line value", k)
		}
	}
	for k, v := range map[string]string{"expires": j.Expires, "signed_at": j.SignedAt, "resolve_at": j.ResolveAt, "sent_at": j.SentAt} {
		if v == "" && k != "expires" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
			return fmt.Errorf("%s %q is not an RFC 3339 time", k, v)
		}
	}
	if j.Credit != "" && strings.ContainsAny(j.Credit, "\x00\n\r") {
		return fmt.Errorf("credit is one line")
	}
	return nil
}

// ---------------------------------------------------------------- placement block

const fenceOpen, fenceClose = "```placement", "```"

// Encode writes a placement as one fenced block of `key = "value"` lines,
// sorted by key: the canonical bytes a signature (M3) will cover. Lists are
// comma-joined (their items are labels, which hold no comma); tags are
// `tags.<key>`.
func (j *Job) Encode() []byte {
	m := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	set("id", j.ID)
	set("name", j.Name)
	set("kind", j.Kind)
	set("asked_by", j.AskedBy)
	set("scope", j.Scope)
	set("area", j.Area)
	set("priority", j.Priority)
	set("needs.tools", strings.Join(j.Needs.Tools, ","))
	set("needs.secrets", strings.Join(j.Needs.Secrets, ","))
	set("needs.qualifications", strings.Join(j.Needs.Qualifications, ","))
	set("needs.repo", j.Needs.Repo)
	set("needs.repo_mode", j.Needs.RepoMode)
	set("effects", strings.Join(j.Effects, ","))
	set("credit", j.Credit)
	set("surface", j.Surface)
	set("profile", j.Profile)
	set("repo", j.Repo)
	set("branch", j.Branch)
	set("base", j.Base)
	set("pin", strings.Join(j.Pin, ","))
	set("expires", j.Expires)
	set("signed_at", j.SignedAt)
	set("prompt", j.Prompt)
	set("office", j.Office)
	set("machine", j.Machine)
	set("region", j.Region)
	set("resolve_at", j.ResolveAt)
	set("sent_at", j.SentAt)
	for k, v := range j.Tags {
		m["tags."+k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(fenceOpen + "\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, strconv.Quote(m[k]))
	}
	b.WriteString(fenceClose + "\n")
	return []byte(b.String())
}

// Decode reads a placement block strictly: one fence, known keys, each once.
func Decode(b []byte) (*Job, error) {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) < 2 || lines[0] != fenceOpen || lines[len(lines)-1] != fenceClose {
		return nil, fmt.Errorf("not one placement block")
	}
	j := &Job{Tags: map[string]string{}}
	seen := map[string]bool{}
	list := func(v string) []string {
		if v == "" {
			return nil
		}
		return strings.Split(v, ",")
	}
	for _, l := range lines[1 : len(lines)-1] {
		k, raw, ok := strings.Cut(l, " = ")
		if !ok {
			return nil, fmt.Errorf("line %q is not key = \"value\"", l)
		}
		if seen[k] {
			return nil, fmt.Errorf("key %s twice", k)
		}
		seen[k] = true
		v, err := strconv.Unquote(raw)
		if err != nil || !strings.HasPrefix(raw, `"`) {
			return nil, fmt.Errorf("key %s: value is not a quoted string", k)
		}
		dst := map[string]*string{"id": &j.ID, "name": &j.Name, "kind": &j.Kind, "asked_by": &j.AskedBy, "scope": &j.Scope,
			"area": &j.Area, "priority": &j.Priority, "needs.repo": &j.Needs.Repo, "needs.repo_mode": &j.Needs.RepoMode,
			"credit": &j.Credit, "surface": &j.Surface, "profile": &j.Profile, "repo": &j.Repo, "branch": &j.Branch,
			"base": &j.Base, "expires": &j.Expires, "signed_at": &j.SignedAt, "prompt": &j.Prompt, "office": &j.Office,
			"machine": &j.Machine, "region": &j.Region, "resolve_at": &j.ResolveAt, "sent_at": &j.SentAt}
		switch {
		case dst[k] != nil:
			*dst[k] = v
		case k == "needs.tools":
			j.Needs.Tools = list(v)
		case k == "needs.secrets":
			j.Needs.Secrets = list(v)
		case k == "needs.qualifications":
			j.Needs.Qualifications = list(v)
		case k == "effects":
			j.Effects = list(v)
		case k == "pin":
			j.Pin = list(v)
		case strings.HasPrefix(k, "tags."):
			j.Tags[k[5:]] = v
		default:
			return nil, fmt.Errorf("unknown key %s", k)
		}
	}
	if err := j.Validate(); err != nil {
		return nil, err
	}
	return j, nil
}

// ---------------------------------------------------------------- fixed checks

// Catalog is what an office publishes about itself: the parts of its record
// the resolver matches against (`office describe --json`).
type Catalog struct {
	Office   string        `json:"office"`
	Region   string        `json:"region"`
	Machine  string        `json:"machine"`
	Owner    []string      `json:"owner"`
	Tier     string        `json:"tier"`
	ActsFor  string        `json:"acts_for"`
	Kinds    []string      `json:"kinds"`
	Profile  string        `json:"profile"`
	Surfaces []string      `json:"surfaces"`
	Holdings Holdings      `json:"holdings"`
	Repos    []RepoHolding `json:"repos"`
	Signers  []string      `json:"signers"`
	Authors  []string      `json:"authors"`
	Default  string        `json:"default"`
	Box      string        `json:"box"`
	BoxPath  string        `json:"box_path"`
	State    *State        `json:"state"`
	Verdicts []Published   `json:"verdicts"`
	Running  int           `json:"running"`
	Pending  int           `json:"pending"`
	Mode     string        `json:"mode"`
}

// Published is an office-wide verdict an office may show until valid_until.
type Published struct {
	Instance   string `json:"instance"`
	Verdict    string `json:"verdict"`
	Reason     string `json:"reason,omitempty"`
	RetryAfter string `json:"retry_after,omitempty"`
	ValidUntil string `json:"valid_until"`
}

// SurfaceName is the surface part of `<transport>:<surface>`.
func SurfaceName(s string) string {
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// SecretScope is the scope a secret holding is tagged with: in
// `<store>:<scope>/<name>`, the part between the colon and the first slash.
func SecretScope(s string) string {
	_, rest, ok := strings.Cut(s, ":")
	if !ok {
		return ""
	}
	sc, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return sc
}

// Mismatch lists why an office can't take a job by what it holds (the
// fixed checks of spec §7.2 step 3 and §8.2), the rules aside. Empty means
// it holds everything the job needs. The office runs the same checks again
// when it accepts.
func Mismatch(j *Job, c *Catalog, reg *Region) []string {
	var why []string
	if !contains(c.Kinds, j.Kind) {
		why = append(why, fmt.Sprintf("doesn't take kind %s", j.Kind))
	}
	if j.Profile != "" && j.Profile != c.Profile {
		why = append(why, fmt.Sprintf("profile %s isn't its own", j.Profile))
	}
	if j.Surface != "" && !contains(c.Surfaces, SurfaceName(j.Surface)) {
		why = append(why, fmt.Sprintf("surface %s isn't its own", j.Surface))
	}
	for _, t := range j.Needs.Tools {
		if !contains(c.Holdings.Tools, t) {
			why = append(why, "lacks tool "+t)
		}
	}
	for _, s := range j.Needs.Secrets {
		if !contains(c.Holdings.Secrets, s) {
			why = append(why, "lacks secret "+s)
		} else if sc := SecretScope(s); sc != "" && sc != j.Scope {
			why = append(why, fmt.Sprintf("secret %s is scope %s only", s, sc))
		}
	}
	for _, q := range j.Needs.Qualifications {
		if !contains(c.Holdings.Qualifications, q) {
			why = append(why, "lacks qualification "+q)
		}
	}
	if j.Needs.Repo != "" {
		var h *RepoHolding
		for i := range c.Repos {
			if c.Repos[i].Repo == j.Needs.Repo {
				h = &c.Repos[i]
			}
		}
		switch {
		case h == nil:
			why = append(why, "repo "+j.Needs.Repo+" isn't on its allow list")
		case j.Needs.RepoMode != "" && !contains(h.Modes, j.Needs.RepoMode):
			why = append(why, fmt.Sprintf("repo %s isn't held in mode %s", j.Needs.Repo, j.Needs.RepoMode))
		case h.Scope != "" && h.Scope != j.Scope:
			why = append(why, fmt.Sprintf("repo %s is scope %s only", j.Needs.Repo, h.Scope))
		}
	}
	for _, e := range j.Effects {
		if reg != nil && contains(reg.ReservedEffects, e) {
			why = append(why, "effect "+e+" is reserved")
		} else if !contains(c.Holdings.Effects, e) {
			why = append(why, "effect "+e+" exceeds its own")
		}
	}
	if len(c.Signers) > 0 && !contains(c.Signers, j.AskedBy) {
		why = append(why, j.AskedBy+" isn't among its signers")
	}
	if j.Credit != "" && !contains(c.Authors, j.Credit) {
		why = append(why, "credit isn't on its authors list")
	}
	if j.Priority != "" && reg != nil && len(reg.Priorities) > 0 && !contains(reg.Priorities, j.Priority) {
		why = append(why, "priority "+j.Priority+" isn't one of the region's")
	}
	return why
}

// CatalogOf is the office's own catalog entry, the same shape the resolver
// reads, so the office's accept check and the resolver's filter are one
// function.
func CatalogOf(c *Config) *Catalog {
	o := c.Office
	cat := &Catalog{Office: o.Name, Region: o.Region, Machine: o.Machine, Owner: o.Owner, Tier: o.Tier,
		ActsFor: o.ActsFor, Kinds: o.Kinds, Profile: o.Profile, Surfaces: o.Surfaces, Holdings: o.Holdings,
		Repos: o.RepoList, Signers: o.SignerNames, Authors: o.AuthorList, Default: o.Rules.Default,
		Box: c.Box(), BoxPath: c.L.BoxDir(c.Box()), Mode: "root"}
	if c.L.Dev {
		cat.Mode = "dev"
	}
	return cat
}

// PriorityRank orders priorities: lower is higher; unknown and none last.
func PriorityRank(reg *Region, p string) int {
	if reg != nil {
		for i, x := range reg.Priorities {
			if x == p {
				return i
			}
		}
		return len(reg.Priorities)
	}
	return 0
}
