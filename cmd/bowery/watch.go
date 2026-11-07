package main

// bowery watch: one background watcher for everything an orchestrator has in
// flight. It polls, and EXITS (waking whoever launched it in the background)
// as soon as something worth acting on changes, printing what changed and the
// whole state. A quiet stretch ends in a heartbeat exit, so a dead watch can't
// hide. Re-arm it with the config updated to what's in flight now.
//
// Each line of the config is "<kind> <key>". Every kind reports one coarse
// value: a state, never progress text, so it changes on transitions and not
// on narration. A value can be marked quiet (it doesn't wake on its own) by
// the kind; the built-in ones mark in-progress states quiet.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const watchUsage = `usage: bowery watch <config> [--poll 30s] [--max 45m]

config lines (# comments; state is kept beside it, <config>.state):
  machine <label>          herdr machine for the agent lines below ("local" = this one)
  repo <owner/name>        GitHub repo for the pr and run lines below
  agent <name>             the agent's herdr status: idle | done | blocked | gone
                           (working is quiet)
  pr <number>              OPEN | MERGED | CLOSED, with /CONFLICTING when it is
  run <workflow> <ref>     the latest run of a workflow on a branch or commit:
                           status/conclusion@sha (queued and in_progress are quiet)
  <kind> <key...>          anything else runs ~/.config/bowery/watch.d/<kind> <key...>
                           with BOWERY_WATCH_MACHINE and BOWERY_WATCH_REPO set; its
                           last line of stdout is the value, "~value" marks it
                           quiet, a non-zero exit is a transient "?"

Exits 0 printing CHANGED: (the lines that changed) and ALL: (every line), or
STILL BLOCKED: on the first poll when an agent is at a dialog, or HEARTBEAT
after --max with nothing worth waking for.`

type watchLine struct {
	kind, key, machine, repo string
}

func cmdWatch(argv []string) {
	var path string
	poll, max := 30*time.Second, 45*time.Minute
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; a {
		case "--poll", "--max":
			if i+1 >= len(argv) {
				die("watch: %s needs a duration", a)
			}
			d, err := time.ParseDuration(argv[i+1])
			if err != nil || d <= 0 {
				die("watch: %s %q: not a duration (30s, 45m)", a, argv[i+1])
			}
			if a == "--poll" {
				poll = d
			} else {
				max = d
			}
			i++
		case "-h", "--help":
			fmt.Println(watchUsage)
			return
		default:
			if strings.HasPrefix(a, "-") || path != "" {
				die("watch: unknown argument %s\n%s", a, watchUsage)
			}
			path = a
		}
	}
	if path == "" {
		die("%s", watchUsage)
	}
	lines, err := readWatchConfig(path)
	if err != nil {
		die("watch: %v", err)
	}
	state := path + ".state"
	prev := map[string]string{}
	if data, err := os.ReadFile(state); err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if k, v, ok := splitState(l); ok {
				prev[k] = v
			}
		}
	}
	first := len(prev) == 0
	start := time.Now()
	for polls := 0; ; polls++ {
		now := snapshot(lines)
		var changed []string
		for _, l := range lines {
			id := l.kind + " " + l.key
			v := now[id]
			old, seen := prev[id]
			if first || !seen || old == v.value || v.quiet || v.value == "?" {
				continue
			}
			changed = append(changed, id+" "+v.value)
		}
		writeState(state, lines, now)
		for k, v := range now {
			if v.value != "?" { // a failed read keeps the last real value
				prev[k] = v.value
			}
		}
		first = false
		if len(changed) > 0 {
			fmt.Println("CHANGED:")
			fmt.Println(strings.Join(changed, "\n"))
			fmt.Println("ALL:")
			printAll(lines, now)
			return
		}
		if polls == 0 { // an agent sitting at a dialog is worth a wake straight away
			var blocked []string
			for _, l := range lines {
				if id := l.kind + " " + l.key; l.kind == "agent" && now[id].value == "blocked" {
					blocked = append(blocked, id+" blocked")
				}
			}
			if len(blocked) > 0 {
				fmt.Println("STILL BLOCKED:")
				fmt.Println(strings.Join(blocked, "\n"))
				return
			}
		}
		if time.Since(start) >= max {
			fmt.Printf("HEARTBEAT (no change in %s):\n", max)
			printAll(lines, now)
			return
		}
		time.Sleep(poll)
	}
}

func readWatchConfig(path string) ([]watchLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []watchLine
	m, repo := "local", ""
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		kind, key, _ := strings.Cut(t, " ")
		key = strings.TrimSpace(key)
		switch kind {
		case "machine":
			m = key
			continue
		case "repo":
			repo = key
			continue
		}
		if key == "" {
			return nil, fmt.Errorf("%s:%d: %q needs a key", path, n, kind)
		}
		out = append(out, watchLine{kind: kind, key: key, machine: m, repo: repo})
	}
	return out, sc.Err()
}

type watchValue struct {
	value string
	quiet bool
}

func snapshot(lines []watchLine) map[string]watchValue {
	out := map[string]watchValue{}
	agentsOn := map[string]map[string]string{} // machine -> name -> status, one list per machine
	for _, l := range lines {
		id := l.kind + " " + l.key
		switch l.kind {
		case "agent":
			st, ok := agentsOn[l.machine]
			if !ok {
				st = agentStatuses(l.machine)
				agentsOn[l.machine] = st
			}
			switch v, ok := st[l.key]; {
			case st == nil:
				out[id] = watchValue{value: "?"}
			case !ok:
				out[id] = watchValue{value: "gone"}
			default:
				out[id] = watchValue{value: v, quiet: v == "working"}
			}
		case "pr":
			out[id] = prValue(l)
		case "run":
			out[id] = runValue(l)
		default:
			out[id] = pluginValue(l)
		}
	}
	return out
}

// agentStatuses is every agent's status on a machine, or nil when herdr
// can't be read (a transient "?", never "gone").
func agentStatuses(label string) map[string]string {
	m := label
	if m == "local" {
		m = ""
	}
	saved := machine
	machine = m
	defer func() { machine = saved }()
	ags, err := agents()
	if err != nil {
		return nil
	}
	refreshSessions(ags)
	out := map[string]string{}
	for _, a := range ags {
		out[a.Name] = a.AgentStatus
	}
	return out
}

func ghJSON(out any, args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Env = append(os.Environ(), "MISE_QUIET=1")
	b, err := cmd.Output()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func prValue(l watchLine) watchValue {
	if l.repo == "" {
		return watchValue{value: "no-repo"}
	}
	var p struct {
		State     string `json:"state"`
		Mergeable string `json:"mergeable"`
	}
	if err := ghJSON(&p, "pr", "view", l.key, "-R", l.repo, "--json", "state,mergeable"); err != nil {
		return watchValue{value: "?"}
	}
	v := p.State
	if p.State == "OPEN" && p.Mergeable == "CONFLICTING" { // UNKNOWN after every push is noise
		v += "/CONFLICTING"
	}
	return watchValue{value: v}
}

func runValue(l watchLine) watchValue {
	if l.repo == "" {
		return watchValue{value: "no-repo"}
	}
	wf, ref, ok := strings.Cut(l.key, " ")
	if !ok {
		return watchValue{value: "bad-line"}
	}
	args := []string{"run", "list", "-R", l.repo, "--workflow", wf, "-L", "1", "--json", "status,conclusion,headSha"}
	if len(ref) >= 7 && isHex(ref) {
		args = append(args, "--commit", ref)
	} else {
		args = append(args, "--branch", ref)
	}
	var runs []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadSha    string `json:"headSha"`
	}
	if err := ghJSON(&runs, args...); err != nil {
		return watchValue{value: "?"}
	}
	if len(runs) == 0 {
		return watchValue{value: "none"}
	}
	r := runs[0]
	sha := r.HeadSha
	if len(sha) > 9 {
		sha = sha[:9]
	}
	quiet := r.Status == "queued" || r.Status == "in_progress" || r.Status == "pending" || r.Status == "waiting"
	return watchValue{value: fmt.Sprintf("%s/%s@%s", r.Status, r.Conclusion, sha), quiet: quiet}
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func pluginValue(l watchLine) watchValue {
	if !nameRE.MatchString(l.kind) {
		return watchValue{value: "unknown-kind"}
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home(), ".config")
	}
	p := filepath.Join(base, "bowery", "watch.d", l.kind)
	if _, err := os.Stat(p); err != nil {
		return watchValue{value: "unknown-kind"}
	}
	cmd := exec.Command(p, strings.Fields(l.key)...)
	cmd.Env = append(os.Environ(), "BOWERY_WATCH_MACHINE="+l.machine, "BOWERY_WATCH_REPO="+l.repo)
	b, err := cmd.Output()
	if err != nil {
		return watchValue{value: "?"}
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	v := strings.TrimSpace(lines[len(lines)-1]) // a tool's notice can come first
	if v == "" {
		return watchValue{value: "?"}
	}
	if strings.HasPrefix(v, "~") {
		return watchValue{value: strings.TrimPrefix(v, "~"), quiet: true}
	}
	return watchValue{value: v}
}

func splitState(l string) (string, string, bool) {
	parts := strings.SplitN(l, "\t", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func writeState(path string, lines []watchLine, now map[string]watchValue) {
	var b strings.Builder
	for _, l := range lines {
		id := l.kind + " " + l.key
		v := now[id].value
		if v == "?" { // keep the last real value through a failed read
			continue
		}
		b.WriteString(id + "\t" + v + "\n")
	}
	os.WriteFile(path, []byte(b.String()), 0o644)
}

func printAll(lines []watchLine, now map[string]watchValue) {
	for _, l := range lines {
		id := l.kind + " " + l.key
		v := now[id]
		s := v.value
		if v.quiet {
			s += " (quiet)"
		}
		if l.kind == "agent" && l.machine != "local" {
			s += " [" + l.machine + "]"
		}
		fmt.Println(id + " " + s)
	}
}
