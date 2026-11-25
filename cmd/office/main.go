// Command office is the office host: it opens and closes an office, says
// what the office holds and what its rules say (describe, and the probe the
// resolver asks), runs the office's pull job over its inbox, and reads its
// log. It is the final judge of every placement into its office.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Techneosis/the-bowery/internal/offices"
)

var version = ""

const usage = `office: the office host. Opens and closes offices, judges placements, keeps the log.

  office ls [--json]                       the offices on this machine
  office describe <office> [--json]        what it holds, its state, its published verdicts
  office describe <office> --probe         judge the job on stdin (JSON) at the place phase
  office open <office> [--until T] [--kinds K,..] [--scope S]
  office close <office> [--until T] [--kinds K,..] [--scope S]
  office clear <office>                    drop the owner's override: the rules decide
  office pull <office> [--json]            one pass over the inbox: accept, refuse or keep waiting
  office pull <office> --loop [--interval 1s] [--backstop 30s] [--for DUR]
                                           the pull job: wait on the inbox, pass, repeat
  office log <office> [--since DUR] [--json]
  office check <office>                    the install checks: plugins, contracts, phases, schemas, tier
  office version

--until takes an RFC 3339 time or a duration from now (90m, 24h).

Root mode reads /usr/local/etc/offices and /var/lib/offices, and checks that
config and plugins are root-owned and that the pull job runs as the office's
user. OFFICE_ROOT=<dir> is dev mode: everything under <dir>, as you, and every
command says what it isn't checking. OFFICE_NOW (dev mode only) fixes the clock.

Exit: 0 done, 1 error.`

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "office: "+format+"\n", a...)
	os.Exit(1)
}

func printJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		die("%v", err)
	}
	fmt.Println(string(b))
}

var layout offices.Layout

func main() {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(usage)
		return
	}
	if args[0] == "version" || args[0] == "--version" {
		v := version
		if v == "" {
			v = "dev"
		}
		fmt.Printf("office %s (%s/%s)\n", v, runtime.GOOS, runtime.GOARCH)
		return
	}
	layout = offices.DetectLayout()
	if n := layout.DevNotice(); n != "" {
		fmt.Fprintln(os.Stderr, "office: "+n)
	}
	if os.Getenv("OFFICE_NOW") != "" && !layout.Dev {
		die("OFFICE_NOW is for dev mode only")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "ls":
		cmdLs(rest)
	case "describe":
		cmdDescribe(rest)
	case "open", "close", "clear":
		cmdFlip(cmd, rest)
	case "pull":
		cmdPull(rest)
	case "log":
		cmdLog(rest)
	case "check":
		cmdCheck(rest)
	default:
		die("unknown command %s (office --help)", cmd)
	}
}

// flags is a tiny parser: the office name first, then --key value pairs and
// bare --switches.
type flags struct {
	office string
	vals   map[string]string
	bools  map[string]bool
}

func parse(args []string, valued []string, switches []string) flags {
	f := flags{vals: map[string]string{}, bools: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case contains(valued, a):
			if i+1 >= len(args) {
				die("%s needs a value", a)
			}
			f.vals[a] = args[i+1]
			i++
		case contains(switches, a):
			f.bools[a] = true
		case strings.HasPrefix(a, "-"):
			die("unknown option %s", a)
		case f.office == "":
			f.office = a
		default:
			die("unexpected %s", a)
		}
	}
	return f
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func need(f flags, what string) {
	if f.office == "" {
		die("%s: which office?", what)
	}
}

func now() time.Time {
	if v := os.Getenv("OFFICE_NOW"); v != "" && layout.Dev {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			die("OFFICE_NOW %q isn't RFC 3339", v)
		}
		return t
	}
	return time.Now()
}

func load(office string) *offices.Config {
	c, err := offices.Load(layout, office)
	if err != nil {
		die("%v", err)
	}
	return c
}

func evaluator(c *offices.Config, component string) *offices.Evaluator {
	ev := offices.NewEvaluator(c, component)
	ev.Clock = now
	return ev
}

func transport(c *offices.Config, poll time.Duration) offices.Transport {
	t, err := offices.NewTransport(c.Machine.Transport, layout, poll)
	if err != nil {
		die("%v", err)
	}
	return t
}

// ---------------------------------------------------------------- ls, describe

func catalog(c *offices.Config) *offices.Catalog {
	cat := offices.CatalogOf(c)
	st, err := offices.ReadState(layout, c.Office.Name)
	if err != nil {
		die("%v", err)
	}
	cat.State = st
	cat.Verdicts = offices.ReadPublished(layout, c.Office.Name, now())
	if l, err := transport(c, 0).Inbox(c.Box()).List(); err == nil {
		cat.Pending = len(l)
	}
	return cat
}

func cmdLs(args []string) {
	f := parse(args, nil, []string{"--json"})
	if f.office != "" {
		die("ls takes no office")
	}
	ents, err := os.ReadDir(layout.Etc)
	if err != nil {
		die("%v", err)
	}
	var cats []*offices.Catalog
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(layout.OfficeFile(e.Name())); err != nil {
			continue
		}
		cats = append(cats, catalog(load(e.Name())))
	}
	if f.bools["--json"] {
		if cats == nil {
			cats = []*offices.Catalog{}
		}
		printJSON(cats)
		return
	}
	for _, c := range cats {
		fmt.Printf("%-28s %-8s %s  kinds=%s tools=%s pending=%d%s\n", c.Office, stateWord(c), c.Region,
			strings.Join(c.Kinds, ","), strings.Join(c.Holdings.Tools, ","), c.Pending, verdictWords(c.Verdicts))
	}
}

func stateWord(c *offices.Catalog) string {
	st := c.State
	if st == nil {
		return "rules"
	}
	if st.Until != "" {
		if t, err := time.Parse(time.RFC3339, st.Until); err == nil && !now().Before(t) {
			return "rules"
		}
	}
	if len(st.Kinds) > 0 || st.Scope != "" {
		return st.State + "*"
	}
	return st.State
}

func verdictWords(vs []offices.Published) string {
	var b strings.Builder
	for _, v := range vs {
		fmt.Fprintf(&b, "  %s=%s", v.Instance, v.Verdict)
		if v.Reason != "" {
			fmt.Fprintf(&b, "(%s)", v.Reason)
		}
	}
	return b.String()
}

func cmdDescribe(args []string) {
	f := parse(args, nil, []string{"--json", "--probe"})
	need(f, "describe")
	c := load(f.office)
	if f.bools["--probe"] {
		probe(c)
		return
	}
	cat := catalog(c)
	if f.bools["--json"] {
		printJSON(cat)
		return
	}
	fmt.Printf("office   %s (region %s, machine %s, tier %s, %s mode)\n", cat.Office, cat.Region, cat.Machine, cat.Tier, cat.Mode)
	fmt.Printf("owner    %s\n", strings.Join(cat.Owner, ", "))
	fmt.Printf("kinds    %s\n", strings.Join(cat.Kinds, ", "))
	fmt.Printf("profile  %s; surfaces %s\n", cat.Profile, strings.Join(cat.Surfaces, ", "))
	fmt.Printf("holds    tools=%s secrets=%s qualifications=%s effects=%s\n", strings.Join(cat.Holdings.Tools, ","),
		strings.Join(cat.Holdings.Secrets, ","), strings.Join(cat.Holdings.Qualifications, ","), strings.Join(cat.Holdings.Effects, ","))
	for _, r := range cat.Repos {
		fmt.Printf("repo     %s modes=%s%s%s\n", r.Repo, strings.Join(r.Modes, ","), opt(" house=", r.House), opt(" scope=", r.Scope))
	}
	if cat.State == nil {
		fmt.Printf("state    no override: the rules decide (default %s)\n", cat.Default)
	} else {
		fmt.Printf("state    %s by %s at %s%s%s%s\n", cat.State.State, cat.State.By, cat.State.At, opt(" until ", cat.State.Until),
			opt(" kinds ", strings.Join(cat.State.Kinds, ",")), opt(" scope ", cat.State.Scope))
	}
	for _, v := range cat.Verdicts {
		fmt.Printf("verdict  %s %s%s until %s\n", v.Instance, v.Verdict, opt(": ", v.Reason), v.ValidUntil)
	}
	fmt.Printf("inbox    %s (%d waiting)\n", cat.Box, cat.Pending)
}

func opt(prefix, v string) string {
	if v == "" {
		return ""
	}
	return prefix + v
}

// probe judges the job on stdin at the place phase, as the office itself
// would, for the resolver. Only the rules and the owner's state: the
// holdings are the resolver's to match from describe.
func probe(c *offices.Config) {
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		die("%v", err)
	}
	j := &offices.Job{}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(j); err != nil {
		die("the probe's job doesn't decode: %v", err)
	}
	if err := j.Validate(); err != nil {
		die("the probe's job: %v", err)
	}
	res, err := evaluator(c, "resolver-probe").Evaluate("place", j)
	if err != nil {
		fmt.Fprintf(os.Stderr, "office: %v\n", err)
	}
	printJSON(map[string]any{"office": c.Office.Name, "verdict": res.Verdict, "reason": res.Reason,
		"retry_after": res.RetryAfter, "by": res.By, "instances": res.Instances})
}

// ---------------------------------------------------------------- open, close

func cmdFlip(cmd string, args []string) {
	valued := []string{"--until", "--kinds", "--scope"}
	if cmd == "clear" {
		valued = nil
	}
	f := parse(args, valued, []string{"--now"})
	need(f, cmd)
	if f.bools["--now"] {
		die("close --now stops running agents, which comes with the run phase; this office host doesn't track running agents yet")
	}
	c := load(f.office)
	me := offices.Whoami()
	if !layout.Dev && !contains(c.Office.Owner, me) {
		die("%s isn't an owner of %s (owners: %s)", me, c.Office.Name, strings.Join(c.Office.Owner, ", "))
	}
	t := now().UTC()
	entry := offices.Entry{"event": "state", "by": me}
	if cmd == "clear" {
		if err := offices.WriteState(layout, c.Office.Name, nil); err != nil {
			die("%v", err)
		}
		entry["state"] = "rules"
	} else {
		st := &offices.State{State: map[string]string{"open": "open", "close": "closed"}[cmd], By: me, At: t.Format(time.RFC3339)}
		if u := f.vals["--until"]; u != "" {
			if d, err := time.ParseDuration(u); err == nil {
				st.Until = t.Add(d).Format(time.RFC3339)
			} else if tt, err := time.Parse(time.RFC3339, u); err == nil {
				st.Until = tt.UTC().Format(time.RFC3339)
			} else {
				die("--until takes an RFC 3339 time or a duration (got %q)", u)
			}
		}
		if k := f.vals["--kinds"]; k != "" {
			st.Kinds = strings.Split(k, ",")
		}
		st.Scope = f.vals["--scope"]
		if err := offices.WriteState(layout, c.Office.Name, st); err != nil {
			die("%v", err)
		}
		entry["state"], entry["until"], entry["kinds"], entry["scope"] = st.State, st.Until, st.Kinds, st.Scope
	}
	if err := offices.Append(layout, c.Office.Name, entry); err != nil {
		die("the state changed but the log can't be written: %v", err)
	}
	fmt.Printf("%s: %s\n", c.Office.Name, map[string]string{"open": "open", "close": "closed", "clear": "back to its rules"}[cmd])
}

// ---------------------------------------------------------------- pull

// checkRunsAsOffice: in root mode the pull job is the office's user and
// nobody else.
func checkRunsAsOffice(c *offices.Config) {
	if layout.Dev {
		return
	}
	u, err := user.Lookup(c.Office.User)
	if err != nil {
		die("there is no user %s on this machine: %v", c.Office.User, err)
	}
	if strconv.Itoa(os.Getuid()) != u.Uid {
		die("the pull job for %s runs as %s, not uid %d", c.Office.Name, c.Office.User, os.Getuid())
	}
}

func cmdPull(args []string) {
	f := parse(args, []string{"--interval", "--backstop", "--for"}, []string{"--json", "--loop"})
	need(f, "pull")
	c := load(f.office)
	checkRunsAsOffice(c)
	dur := func(k string, def time.Duration) time.Duration {
		v, ok := f.vals[k]
		if !ok {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			die("%s takes a duration (got %q)", k, v)
		}
		return d
	}
	interval, backstop := dur("--interval", time.Second), dur("--backstop", 30*time.Second)
	t := transport(c, interval)
	in := t.Inbox(c.Box())
	report := func(rep *offices.PullReport) {
		if f.bools["--json"] {
			printJSON(rep)
			return
		}
		for _, a := range rep.Accepted {
			if a.Exit != nil {
				fmt.Printf("accepted %s, started through %s (exit %d)\n", a.ID, a.Surface, *a.Exit)
			} else {
				fmt.Printf("accepted %s, not started: %s\n", a.ID, a.Reason)
			}
		}
		for _, w := range rep.Waiting {
			fmt.Printf("waiting  %s: %s%s\n", w.ID, w.Reason, opt(", retry after ", w.RetryAfter))
		}
		for _, r := range rep.Refused {
			fmt.Printf("refused  %s: %s\n", r.ID, r.Reason)
		}
		for _, d := range rep.Dropped {
			fmt.Printf("dropped  %s: %s\n", d.ID, d.Reason)
		}
	}
	if !f.bools["--loop"] {
		rep, err := offices.Pull(c, evaluator(c, "pull"), t, in, true)
		if err != nil {
			die("%v", err)
		}
		report(rep)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, ok := f.vals["--for"]; ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dur("--for", 0))
		defer cancel()
	}
	for {
		c = load(f.office) // a re-installed record or machine file takes effect on the next pass
		rep, err := offices.Pull(c, evaluator(c, "pull"), t, in, false)
		if err != nil {
			die("%v", err)
		}
		if len(rep.Accepted)+len(rep.Refused)+len(rep.Dropped) > 0 {
			report(rep)
		}
		wait := backstop
		if rep.NextRetry != "" {
			if ra, err := time.Parse(time.RFC3339Nano, rep.NextRetry); err == nil {
				if d := time.Until(ra); d < wait {
					wait = max(d, 0)
				}
			}
		}
		// Wait for mail, an event (the owner's state, a config file) or the
		// next retry, whichever is first, never past the backstop. A
		// long-polling transport blocks inside Wait; the directory one
		// looks every --interval.
		events, deadline := c.Events(), time.Now().Add(wait)
		for {
			step := min(interval, time.Until(deadline))
			if step <= 0 {
				break
			}
			changed, err := in.Wait(ctx, step)
			if err != nil {
				return
			}
			if changed || c.Events() != events {
				break
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// ---------------------------------------------------------------- log

func cmdLog(args []string) {
	f := parse(args, []string{"--since"}, []string{"--json"})
	need(f, "log")
	c := load(f.office)
	var since time.Time
	if s := f.vals["--since"]; s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			die("--since takes a duration (got %q)", s)
		}
		since = now().Add(-d)
	}
	es, err := offices.ReadLog(layout, c.Office.Name, since)
	if err != nil {
		die("%v", err)
	}
	if f.bools["--json"] {
		for _, e := range es {
			printJSON(e)
		}
		return
	}
	for _, e := range es {
		fmt.Println(logLine(e))
	}
}

func logLine(e offices.Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v %-14v", e["time"], e["event"])
	keys := make([]string, 0, len(e))
	for k := range e {
		if k != "time" && k != "event" && k != "office" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := e[k]
		if v == nil || v == "" || v == float64(0) && k != "exit" {
			continue
		}
		switch x := v.(type) {
		case string:
			if strings.ContainsAny(x, " =") {
				fmt.Fprintf(&b, " %s=%q", k, x)
			} else {
				fmt.Fprintf(&b, " %s=%s", k, x)
			}
		default:
			j, _ := json.Marshal(x)
			fmt.Fprintf(&b, " %s=%s", k, j)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------- check

// cmdCheck runs the install checks (spec §5.1, §4.2) over an office's
// record: each instance's plugin is installed, pinned, speaks its contract
// and runs at its phases, its config fits the schema, and the holdings fit
// the tier. Every problem is listed; any one fails the check.
func cmdCheck(args []string) {
	f := parse(args, nil, nil)
	need(f, "check")
	c := load(f.office)
	ev := evaluator(c, "install")
	var probs []string
	seen := map[string]bool{}
	for _, in := range ev.Instances() {
		if seen[in.Name] {
			probs = append(probs, fmt.Sprintf("instance %s: the name is used twice", in.Name))
		}
		seen[in.Name] = true
		if !offices.NameRE.MatchString(in.Name) {
			probs = append(probs, fmt.Sprintf("instance %q: not a valid name", in.Name))
		}
		if in.OnClosed != "" && !contains([]string{"yield", "finish-turn", "stop-next-turn"}, in.OnClosed) {
			probs = append(probs, fmt.Sprintf("instance %s: on_closed %q", in.Name, in.OnClosed))
		}
		d, err := ev.Describe(in.Plugin)
		if err != nil {
			probs = append(probs, fmt.Sprintf("instance %s: %v", in.Name, err))
			continue
		}
		contract := in.Contract
		if contract == 0 {
			contract = 1
		}
		if contract != offices.Contract || !containsInt(d.Contracts, contract) || !containsInt(c.Machine.Plugins[in.Plugin].Contracts, contract) {
			probs = append(probs, fmt.Sprintf("instance %s: contract %d isn't spoken by the core, the plugin and its machine-file entry", in.Name, contract))
		}
		for _, ph := range in.At {
			if !contains(d.Phases, ph) {
				probs = append(probs, fmt.Sprintf("instance %s: plugin %s doesn't run at %s", in.Name, in.Plugin, ph))
			}
		}
		if len(d.ConfigSchema) > 0 {
			var schema any
			_ = json.Unmarshal(d.ConfigSchema, &schema)
			cb, _ := json.Marshal(nz(in.Config))
			var cfg any
			_ = json.Unmarshal(cb, &cfg)
			if err := offices.ValidateSchema(schema, cfg); err != nil {
				probs = append(probs, fmt.Sprintf("instance %s: %v", in.Name, err))
			}
		}
	}
	o := c.Office
	if o.Tier != "" {
		tier, ok := c.Region.Tiers[o.Tier]
		if !ok {
			probs = append(probs, fmt.Sprintf("tier %s isn't one of the region's", o.Tier))
		} else {
			for _, e := range o.Holdings.Effects {
				if !contains(tier.Effects, e) {
					probs = append(probs, fmt.Sprintf("effect %s exceeds tier %s", e, o.Tier))
				}
			}
			if len(tier.Holds) > 0 {
				for k, has := range map[string]bool{"tools": len(o.Holdings.Tools) > 0, "repos": o.Holdings.Repos != "",
					"secrets": len(o.Holdings.Secrets) > 0, "qualifications": len(o.Holdings.Qualifications) > 0} {
					if has && !contains(tier.Holds, k) {
						probs = append(probs, fmt.Sprintf("tier %s may not hold %s", o.Tier, k))
					}
				}
			}
		}
	}
	for _, s := range o.Surfaces {
		if _, ok := c.Machine.Surfaces[s]; !ok {
			probs = append(probs, fmt.Sprintf("surface %s has no command in the machine file", s))
		}
	}
	if max := c.Region.Naming.UserMax; len(o.Name) > max || len(offices.BoxName(c.Region, o.Name)) > max {
		probs = append(probs, fmt.Sprintf("the name or its box is longer than user_max %d", max))
	}
	if len(probs) > 0 {
		for _, p := range probs {
			fmt.Fprintln(os.Stderr, "office: "+p)
		}
		die("%s: %d problem(s); the record would be refused", o.Name, len(probs))
	}
	fmt.Printf("%s: ok (%d rule instances, box %s at %s)\n", o.Name, len(ev.Instances()), c.Box(), filepath.Clean(layout.BoxDir(c.Box())))
}

func nz(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
