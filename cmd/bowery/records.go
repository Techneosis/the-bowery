package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A dispatch record is how `done` knows what bowery made for an agent: the
// tab or workspace ("owns": every pane in it is the agent's) and its
// worktrees. One JSON file per agent per server:
// <state>/<name>.<first 8 hex of sha1(server key)>.json, the server key being
// the local socket path or "machine:<label>".

type RecWorktree struct {
	Path   string `json:"path"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

type Owns struct {
	Kind string `json:"kind"` // "tab" | "workspace"
	ID   string `json:"id"`
}

type Record struct {
	Name        string        `json:"name"`
	Socket      *string       `json:"socket"`
	Placement   string        `json:"placement"`
	Kind        string        `json:"kind"`
	Started     string        `json:"started"`
	Machine     string        `json:"machine,omitempty"`
	Host        string        `json:"host,omitempty"`
	As          string        `json:"as,omitempty"`
	Worktree    string        `json:"worktree,omitempty"`
	Branch      string        `json:"branch,omitempty"`
	Repo        string        `json:"repo,omitempty"`
	Base        string        `json:"base,omitempty"`
	PaneID      string        `json:"pane_id"`
	TerminalID  string        `json:"terminal_id"`
	WorkspaceID string        `json:"workspace_id"`
	TabID       string        `json:"tab_id"`
	CreatedPane bool          `json:"created_pane"`
	Sound       string        `json:"sound"`
	Worktrees   []RecWorktree `json:"worktrees"`
	Owns        *Owns         `json:"owns,omitempty"`
	// What resume needs to start the agent again as it was: its session
	// (kept fresh while it lives), the directory it was started in (claude
	// files sessions per directory) and its flags. Older records lack them.
	Session string `json:"session,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Gated   bool   `json:"gated,omitempty"`
	Model   string `json:"model,omitempty"`
	Resumed string `json:"resumed,omitempty"` // the last resume
	Resumes int    `json:"resumes,omitempty"` // how many times
}

func stateDir() string {
	d := os.Getenv("BOWERY_STATE")
	if d == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			base = filepath.Join(home(), ".local", "state")
		}
		d = filepath.Join(base, "bowery")
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		die("cannot make the state directory %s: %v", d, err)
	}
	return d
}

func serverKey() string {
	if machine != "" {
		return "machine:" + machine
	}
	return socketPath()
}

func serverTag() string {
	h := sha1.Sum([]byte(serverKey()))
	return hex.EncodeToString(h[:])[:8]
}

// Agent names are per server: a test session must not read the real one's,
// and a machine's records are filed apart from the local server's.
func recordPath(name string) string {
	return filepath.Join(stateDir(), name+"."+serverTag()+".json")
}

func readRecord(path string) *Record {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r Record
	if json.Unmarshal(b, &r) != nil || r.Name == "" {
		return nil
	}
	return &r
}

func loadRecord(name string) *Record { return readRecord(recordPath(name)) }

func allRecords() []*Record {
	files, _ := filepath.Glob(filepath.Join(stateDir(), "*."+serverTag()+".json"))
	sort.Strings(files)
	var out []*Record
	for _, f := range files {
		if r := readRecord(f); r != nil {
			out = append(out, r)
		}
	}
	return out
}

func writeRecord(r *Record) error {
	if r.Worktrees == nil {
		r.Worktrees = []RecWorktree{}
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	path := recordPath(r.Name)
	// A temp name of its own, so two writers never share one.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr == nil {
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

func dropRecord(name string) {
	_ = os.Remove(recordPath(name))
}

// recWorktrees is every worktree a record claims: --worktree's,
// --also-worktree's, adopted.
func recWorktrees(r *Record) []RecWorktree {
	if r == nil {
		return nil
	}
	out := append([]RecWorktree(nil), r.Worktrees...)
	if r.Worktree != "" {
		for _, w := range out {
			if w.Path == r.Worktree {
				return out
			}
		}
		out = append([]RecWorktree{{Path: r.Worktree, Repo: r.Repo, Branch: r.Branch}}, out...)
	}
	return out
}

// recOwns is the tab/workspace bowery made for this agent. Records written
// before "owns" existed are read by their placement.
func recOwns(r *Record) *Owns {
	switch {
	case r == nil:
		return nil
	case r.Owns != nil:
		return r.Owns
	case r.CreatedPane && (r.Placement == "swarm" || r.Placement == "tab"):
		return &Owns{Kind: "tab", ID: r.TabID}
	case r.CreatedPane && r.Placement == "workspace":
		return &Owns{Kind: "workspace", ID: r.WorkspaceID}
	}
	return nil
}

// machineRecords is every remote dispatch record (any machine), optionally
// only those for one agent name.
func machineRecords(name string) []*Record {
	pat := "*.*.json"
	if name != "" {
		pat = name + ".*.json"
	}
	files, _ := filepath.Glob(filepath.Join(stateDir(), pat))
	sort.Strings(files)
	var out []*Record
	for _, f := range files {
		r := readRecord(f)
		if r != nil && r.Machine != "" && (name == "" || r.Name == name) {
			out = append(out, r)
		}
	}
	return out
}

func recordedMachines() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range machineRecords("") {
		if !seen[r.Machine] {
			seen[r.Machine] = true
			out = append(out, r.Machine)
		}
	}
	sort.Strings(out)
	return out
}

// recordLive: the agent is still up, or its pane is (an agent that exited
// to a shell).
func recordLive(r *Record, ags []Agent, ps []Pane) bool {
	for _, a := range ags {
		if a.Name == r.Name {
			return true
		}
	}
	if r.TerminalID != "" {
		for _, p := range ps {
			if p.TerminalID == r.TerminalID {
				return true
			}
		}
	}
	return false
}

type claim struct {
	name string
	live bool
}

// worktreeClaims maps each recorded worktree (normalised) to who claims it.
func worktreeClaims(ags []Agent, ps []Pane) map[string][]claim {
	out := map[string][]claim{}
	for _, r := range allRecords() {
		live := recordLive(r, ags, ps)
		for _, w := range recWorktrees(r) {
			k := norm(w.Path)
			out[k] = append(out[k], claim{r.Name, live})
		}
	}
	return out
}

// forgetWorktree strikes a removed worktree from any (dead) record that
// claimed it.
func forgetWorktree(path string) error {
	for _, r := range allRecords() {
		have := recWorktrees(r)
		var keep []RecWorktree
		for _, w := range have {
			if norm(w.Path) != path {
				keep = append(keep, w)
			}
		}
		if len(keep) == len(have) {
			continue
		}
		r.Worktrees = keep
		if r.Worktree != "" && norm(r.Worktree) == path {
			r.Worktree, r.Branch = "", ""
		}
		if err := writeRecord(r); err != nil {
			return err
		}
	}
	return nil
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05+00:00") }

// popMachine strips `--machine <label>` out of a subcommand's arguments.
func popMachine(argv []string) (string, []string) {
	for i, a := range argv {
		if a == "--machine" {
			if i+1 >= len(argv) {
				die("--machine needs a value")
			}
			rest := append(append([]string(nil), argv[:i]...), argv[i+2:]...)
			return argv[i+1], rest
		}
	}
	return "", argv
}

// pickMachine points at the server <target> lives on, before anything looks
// it up. --machine wins. Otherwise a name known to the local server (a record
// or a live agent) is local; failing that, the one machine with a dispatch
// record by that name. Pane ids and paths are local without --machine: they
// mean nothing across servers.
func pickMachine(target, explicit string) error {
	if explicit != "" {
		who := ""
		if nameRE.MatchString(target) {
			for _, r := range machineRecords(target) {
				if r.Machine == explicit {
					who = r.As
					break
				}
			}
		}
		useMachine(explicit, who)
		return nil
	}
	useMachine("", "")
	if !nameRE.MatchString(target) {
		return nil
	}
	if r := loadRecord(target); r != nil {
		useMachine("", r.As) // a local agent with an identity: its git runs as them
		return nil
	}
	ags, err := agents()
	if err != nil {
		return err
	}
	for _, a := range ags {
		if a.Name == target {
			return nil
		}
	}
	recs := machineRecords(target)
	ms := map[string]bool{}
	for _, r := range recs {
		ms[r.Machine] = true
	}
	if len(ms) > 1 {
		names := make([]string, 0, len(ms))
		for m := range ms {
			names = append(names, m)
		}
		sort.Strings(names)
		return refuse("%s was dispatched to more than one machine (%s); pass --machine", target, strings.Join(names, ", "))
	}
	if len(recs) > 0 {
		useMachine(recs[0].Machine, recs[0].As)
	}
	return nil
}

// freshSession is the session id herdr reports now for r's live agent, or ""
// when there is none to take. Only the agent in the record's own terminal
// counts: a namesake elsewhere is not this agent. An older record without a
// terminal id is matched by pane instead; with neither to compare, nothing
// is taken.
func freshSession(r *Record, ags []Agent) string {
	for _, a := range ags {
		if a.Name != r.Name || a.SessionID() == "" {
			continue
		}
		switch {
		case r.TerminalID != "" && a.TerminalID != "":
			if a.TerminalID != r.TerminalID {
				continue
			}
		case r.PaneID != "" && a.PaneID != "":
			if a.PaneID != r.PaneID {
				continue
			}
		default:
			continue
		}
		return a.SessionID()
	}
	return ""
}

// withFreshSessions is every record on this server, each with the session
// its live agent has now (a /clear starts a new one). It changes no file.
func withFreshSessions(ags []Agent) []*Record {
	recs := allRecords()
	for _, r := range recs {
		if s := freshSession(r, ags); s != "" {
			r.Session = s
		}
	}
	return recs
}

// refreshSessions writes those fresh session ids into the records, so a
// session is known after its agent dies. Each write re-reads the record
// first and gives way if it was dropped or changed meanwhile (a `done` or a
// `resume` in between wins), and a failed write only warns.
func refreshSessions(ags []Agent) {
	for _, r := range allRecords() {
		s := freshSession(r, ags)
		if s == "" || s == r.Session {
			continue
		}
		cur := readRecord(recordPath(r.Name))
		if cur == nil || cur.Session != r.Session || cur.TerminalID != r.TerminalID {
			continue
		}
		cur.Session = s
		if err := writeRecord(cur); err != nil {
			warn("could not update %s's record: %v", r.Name, err)
		}
	}
}
