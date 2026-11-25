package offices

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Contract is the rule contract this core speaks (spec §5.2).
const Contract = 1

const maxStdout = 64 << 10

var Phases = []string{"place", "accept", "run"}

// Description is a plugin's answer to {"contract": 1, "describe": true}.
type Description struct {
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	Contracts    []int           `json:"contracts"`
	Phases       []string        `json:"phases"`
	Facts        []string        `json:"facts"`
	History      string          `json:"history"`
	ConfigSchema json.RawMessage `json:"config_schema"`
}

type Alert struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// Verdict is one rule's answer (spec §5.4).
type Verdict struct {
	Verdict    string `json:"verdict"`
	Reason     string `json:"reason,omitempty"`
	RetryAfter string `json:"retry_after,omitempty"`
	Holds      string `json:"holds,omitempty"`
	ValidUntil string `json:"valid_until,omitempty"`
	Note       string `json:"note,omitempty"`
	Alert      *Alert `json:"alert,omitempty"`
}

// InstanceResult is one instance's part in one evaluation.
type InstanceResult struct {
	Instance      Instance `json:"-"`
	Name          string   `json:"instance"`
	Source        string   `json:"source"`
	Plugin        string   `json:"plugin"`
	PluginVersion string   `json:"plugin_version,omitempty"`
	SHA256        string   `json:"sha256,omitempty"`
	Contract      int      `json:"contract"`
	Verdict       Verdict  `json:"verdict"`
	Failed        string   `json:"failed,omitempty"`
	MS            int64    `json:"ms"`
	Binding       bool     `json:"binding,omitempty"`
	// StateMTime is a state-file rule's file mtime, logged where an exec
	// rule logs its sha256.
	StateMTime string `json:"state_mtime,omitempty"`
}

// Result is the combined answer for one job at one moment.
type Result struct {
	Verdict    string `json:"verdict"`
	Reason     string `json:"reason,omitempty"`
	RetryAfter string `json:"retry_after,omitempty"`
	By         string `json:"by"` // rules, default, owner
	// OnClosed is what this closed result asks of a running job: the
	// closing source's on_closed (spec §4.5).
	OnClosed  string           `json:"on_closed,omitempty"`
	Override  *State           `json:"override,omitempty"` // the owner's state, when it decided
	Instances []InstanceResult `json:"instances"`
}

// Evaluator runs one office's rules. Now is read once per evaluation and
// shared by every instance in it.
type Evaluator struct {
	C         *Config
	Component string // pull, daemon, wrapper, resolver-probe, run
	Clock     func() time.Time
	// Facts is the machine's fact readings; the pull loop keeps one across
	// passes. Nil reads fresh.
	Facts *Facts

	mu    sync.Mutex
	descs map[string]*Description
}

func NewEvaluator(c *Config, component string) *Evaluator {
	return &Evaluator{C: c, Component: component, Clock: time.Now, descs: map[string]*Description{}}
}

// Instances are the rule instances that apply to the office, in the order
// they run: the region's, the machine's, then the office's.
func (e *Evaluator) Instances() []Instance {
	var all []Instance
	all = append(all, e.C.Region.Defaults.Rules...)
	all = append(all, e.C.Machine.Rules...)
	all = append(all, e.C.Office.Rules.Use...)
	return all
}

func (e *Evaluator) timeoutFor(in Instance) time.Duration {
	if in.Timeout != "" {
		return Duration(in.Timeout, 2*time.Second)
	}
	return Duration(e.C.Region.Defaults.RuleTimeout, 2*time.Second)
}

// Evaluate judges one job in one phase, logs every verdict, and combines
// them. The error is a log that couldn't be written; the result is then
// closed, since an unlogged verdict must not admit anything.
func (e *Evaluator) Evaluate(phase string, job *Job) (*Result, error) {
	now := e.Clock().UTC()
	insts := e.Instances()
	names := map[string]int{}
	for _, in := range insts {
		names[in.Name]++
	}
	results := make([]*InstanceResult, len(insts))
	var wg sync.WaitGroup
	for i, in := range insts {
		wg.Add(1)
		go func(i int, in Instance) {
			defer wg.Done()
			if names[in.Name] > 1 {
				results[i] = failed(in, "its name is used by another instance on this office")
				return
			}
			results[i] = e.runInstance(in, phase, job, now)
		}(i, in)
	}
	wg.Wait()

	res := &Result{Instances: []InstanceResult{}}
	for _, r := range results {
		if r != nil {
			res.Instances = append(res.Instances, *r)
		}
	}
	closer := combine(res, e.C.Office.Rules.Default)
	st, err := ReadState(e.C.L, e.C.Office.Name)
	if err != nil {
		res.Verdict, res.Reason, res.RetryAfter, res.By = "closed", "the owner's state can't be read: "+err.Error(), "", "owner"
		closer = nil
	} else if by := applyOverride(res, st, job, now); by != "" {
		closer = nil
		if by == "binding" {
			for i := range res.Instances {
				if r := res.Instances[i]; r.Binding && r.Verdict.Verdict == "closed" {
					in := r.Instance
					closer = &in
					break
				}
			}
		} else {
			res.Override = st
		}
	}
	if res.Verdict == "closed" {
		res.OnClosed = e.C.OnClosedFor(closer)
	}

	jobID := ""
	if job != nil {
		jobID = job.ID
	}
	logErr := e.log(phase, jobID, res, now)
	if logErr != nil {
		res.Verdict, res.Reason, res.RetryAfter = "closed", "the office log can't be written: "+logErr.Error(), ""
	}
	if phase == "place" {
		e.cache(res, now)
	}
	return res, logErr
}

func failed(in Instance, what string) *InstanceResult {
	return &InstanceResult{Instance: in, Name: in.Name, Source: in.source, Plugin: in.Plugin, Contract: contractOf(in),
		Failed: what, Binding: in.Binding,
		Verdict: Verdict{Verdict: "closed", Reason: fmt.Sprintf("rule %s failed: %s", in.Name, what)}}
}

func contractOf(in Instance) int {
	if in.Contract == 0 {
		return 1
	}
	return in.Contract
}

// combine returns the closing instance (the first closed one), if any.
func combine(res *Result, def string) *Instance {
	var closed []InstanceResult
	anyOpen := false
	for _, r := range res.Instances {
		switch r.Verdict.Verdict {
		case "closed":
			closed = append(closed, r)
		case "open":
			anyOpen = true
		}
	}
	switch {
	case len(closed) > 0:
		res.Verdict, res.Reason, res.By = "closed", closed[0].Verdict.Reason, "rules"
		latest := ""
		for _, c := range closed {
			if c.Verdict.RetryAfter == "" {
				latest = ""
				break
			}
			if latest == "" || later(c.Verdict.RetryAfter, latest) {
				latest = c.Verdict.RetryAfter
			}
		}
		res.RetryAfter = latest
		in := closed[0].Instance
		return &in
	case anyOpen:
		res.Verdict, res.By = "open", "rules"
	default:
		res.Verdict, res.By = def, "default"
		if def == "closed" {
			res.Reason = "closed by default: no rule admits it"
		}
	}
	return nil
}

// Earlier says whether RFC 3339 time a is before b, whatever offsets they
// are written in. A time that doesn't parse is never earlier.
func Earlier(a, b string) bool {
	ta, e1 := time.Parse(time.RFC3339Nano, a)
	tb, e2 := time.Parse(time.RFC3339Nano, b)
	return e1 == nil && e2 == nil && ta.Before(tb)
}

func later(a, b string) bool {
	ta, _ := time.Parse(time.RFC3339Nano, a)
	tb, _ := time.Parse(time.RFC3339Nano, b)
	return ta.After(tb)
}

// applyOverride: close beats every rule; open beats every rule but a
// binding one (spec §4.4). It says what decided: "owner", "binding" (an
// open the binding rule kept closed), or "" (no override applies).
func applyOverride(res *Result, st *State, job *Job, now time.Time) string {
	if !st.Applies(job, now) {
		return ""
	}
	if st.State == "closed" {
		res.Verdict, res.By = "closed", "owner"
		res.Reason = "closed by its owner"
		if st.By != "" {
			res.Reason = "closed by its owner (" + st.By + ")"
		}
		res.RetryAfter = st.Until
		return "owner"
	}
	for _, r := range res.Instances {
		if r.Binding && r.Verdict.Verdict == "closed" {
			res.Reason = r.Verdict.Reason + " (binding: the owner's open doesn't override it)"
			res.By = "rules"
			return "binding"
		}
	}
	res.Verdict, res.Reason, res.RetryAfter, res.By = "open", "", "", "owner"
	return "owner"
}

func (e *Evaluator) runInstance(in Instance, phase string, job *Job, now time.Time) *InstanceResult {
	if !NameRE.MatchString(in.Name) {
		return failed(in, "the instance has no valid name")
	}
	p, ok := e.C.Machine.Plugins[in.Plugin]
	if !ok {
		return failed(in, fmt.Sprintf("plugin %s isn't installed on this machine", in.Plugin))
	}
	contract := contractOf(in)
	if contract != Contract {
		return failed(in, fmt.Sprintf("contract %d isn't one this core speaks", contract))
	}
	if p.Kind == "state-file" {
		return e.runStateFile(in, p, phase, now)
	}
	if p.Kind != "exec" {
		return failed(in, fmt.Sprintf("plugin kind %q isn't supported by this office host", p.Kind))
	}
	if !containsInt(p.Contracts, contract) {
		return failed(in, fmt.Sprintf("plugin %s isn't installed for contract %d", in.Plugin, contract))
	}
	if err := checkRootOwned(p.Path); err != nil {
		return failed(in, err.Error())
	}
	body, err := os.ReadFile(p.Path)
	if err != nil {
		return failed(in, "can't read the plugin: "+err.Error())
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if p.SHA256 == "" || !strings.EqualFold(got, p.SHA256) {
		return failed(in, "it doesn't match its pinned sha256")
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.timeoutFor(in))
	defer cancel()
	start := time.Now()
	res := &InstanceResult{Instance: in, Name: in.Name, Source: in.source, Plugin: in.Plugin, SHA256: got,
		Contract: contract, Binding: in.Binding}
	fail := func(what string) *InstanceResult {
		f := failed(in, what)
		f.SHA256, f.PluginVersion, f.MS = got, res.PluginVersion, time.Since(start).Milliseconds()
		return f
	}
	desc, err := e.describe(ctx, in.Plugin, p.Path, got)
	if err != nil {
		return fail(err.Error())
	}
	res.PluginVersion = desc.Version
	if !containsInt(desc.Contracts, contract) {
		return fail(fmt.Sprintf("the plugin doesn't speak contract %d", contract))
	}
	at := in.At
	if len(at) == 0 {
		at = desc.Phases
	}
	for _, ph := range at {
		if !contains(desc.Phases, ph) {
			return fail(fmt.Sprintf("the plugin doesn't run at phase %s", ph))
		}
	}
	if !contains(at, phase) {
		return nil // not this instance's phase
	}
	cfg := normalize(in.Config)
	if len(desc.ConfigSchema) > 0 {
		var schema any
		if err := json.Unmarshal(desc.ConfigSchema, &schema); err != nil {
			return fail("its config_schema doesn't decode")
		}
		if err := ValidateSchema(schema, cfg); err != nil {
			return fail("its config doesn't fit the plugin's schema: " + err.Error())
		}
	}
	req, err := json.Marshal(e.request(phase, in, cfg, job, desc, now))
	if err != nil {
		return fail(err.Error())
	}
	out, err := runPlugin(ctx, p.Path, e.C.L.RuleState(in.Plugin), req, e.timeoutFor(in))
	if err != nil {
		return fail(err.Error())
	}
	v, err := decodeVerdict(out)
	if err != nil {
		return fail(err.Error())
	}
	res.Verdict = *v
	res.MS = time.Since(start).Milliseconds()
	return res
}

func containsInt(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// normalize turns TOML-decoded config into what JSON would decode, so the
// schema sees one set of types.
func normalize(c map[string]any) any {
	if c == nil {
		return map[string]any{}
	}
	b, _ := json.Marshal(c)
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}

func (e *Evaluator) describe(ctx context.Context, plugin, path, sum string) (*Description, error) {
	e.mu.Lock()
	d, ok := e.descs[sum]
	e.mu.Unlock()
	if ok {
		return d, nil
	}
	out, err := runPlugin(ctx, path, e.C.L.RuleState(plugin), []byte(`{"contract": 1, "describe": true}`), 0)
	if err != nil {
		return nil, fmt.Errorf("describe: %v", err)
	}
	d = &Description{}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(d); err != nil {
		return nil, fmt.Errorf("its description doesn't decode: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("its description has trailing data")
	}
	if d.Name == "" || len(d.Contracts) == 0 || len(d.Phases) == 0 {
		return nil, fmt.Errorf("its description lacks name, contracts or phases")
	}
	for _, ph := range d.Phases {
		if !contains(Phases, ph) {
			return nil, fmt.Errorf("its description names an unknown phase %q", ph)
		}
	}
	if d.History != "" {
		if _, err := time.ParseDuration(d.History); err != nil {
			return nil, fmt.Errorf("its history %q isn't a duration", d.History)
		}
	}
	e.mu.Lock()
	e.descs[sum] = d
	e.mu.Unlock()
	return d, nil
}

// Describe asks one installed plugin for its description (for `office check`).
func (e *Evaluator) Describe(plugin string) (*Description, error) {
	p, ok := e.C.Machine.Plugins[plugin]
	if !ok {
		return nil, fmt.Errorf("plugin %s isn't installed on this machine", plugin)
	}
	if p.Kind != "exec" {
		return nil, fmt.Errorf("plugin kind %q isn't supported by this office host", p.Kind)
	}
	body, err := os.ReadFile(p.Path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, p.SHA256) {
		return nil, fmt.Errorf("plugin %s doesn't match its pinned sha256 (it is %s)", plugin, got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), Duration(e.C.Region.Defaults.RuleTimeout, 2*time.Second))
	defer cancel()
	return e.describe(ctx, plugin, p.Path, got)
}

// cappedBuffer keeps the first maxStdout bytes and notes any more.
type cappedBuffer struct {
	b    bytes.Buffer
	over bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := maxStdout - c.b.Len()
	if len(p) > room {
		c.over = true
		if room > 0 {
			c.b.Write(p[:room])
		}
		return len(p), nil
	}
	return c.b.Write(p)
}

// runPlugin runs one exec plugin: the request on stdin, an environment built
// from nothing but its private state directory, the working directory /,
// stdout capped, killed (with its process group) at the deadline.
func runPlugin(ctx context.Context, path, stateDir string, input []byte, _ time.Duration) ([]byte, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("its state directory: %v", err)
	}
	cmd := exec.Command(path)
	cmd.Env = []string{"OFFICE_RULE_STATE=" + stateDir}
	cmd.Dir = "/"
	cmd.Stdin = bytes.NewReader(input)
	out := &cappedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 200 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("it doesn't start: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					return nil, fmt.Errorf("it crashed (%s)", ws.Signal())
				}
				return nil, fmt.Errorf("it exited %d", ee.ExitCode())
			}
			return nil, fmt.Errorf("it failed: %v", err)
		}
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		dl, _ := ctx.Deadline()
		_ = dl
		return nil, fmt.Errorf("no answer in time (killed)")
	}
	if out.over {
		return nil, fmt.Errorf("it wrote more than 64 KiB")
	}
	return out.b.Bytes(), nil
}

func decodeVerdict(out []byte) (*Verdict, error) {
	v := &Verdict{}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, fmt.Errorf("its response doesn't decode: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("its response has trailing data")
	}
	switch v.Verdict {
	case "open", "defer":
	case "closed":
		if v.Reason == "" {
			return nil, fmt.Errorf("it answered closed without a reason")
		}
	case "":
		return nil, fmt.Errorf("its response has no verdict")
	default:
		return nil, fmt.Errorf("its verdict %q isn't open, closed or defer", v.Verdict)
	}
	for k, t := range map[string]string{"retry_after": v.RetryAfter, "valid_until": v.ValidUntil} {
		if t == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, t); err != nil {
			return nil, fmt.Errorf("its %s %q isn't RFC 3339", k, t)
		}
	}
	switch v.Holds {
	case "", "job", "office":
	default:
		return nil, fmt.Errorf("its holds %q isn't office or job", v.Holds)
	}
	if v.Alert != nil && v.Alert.Level != "smoke" && v.Alert.Level != "fire" {
		return nil, fmt.Errorf("its alert level %q isn't smoke or fire", v.Alert.Level)
	}
	return v, nil
}

// ---------------------------------------------------------------- the request

type accepted struct {
	Office   string `json:"office"`
	ID       string `json:"id"`
	Priority string `json:"priority"`
	At       string `json:"at"`
}

func (e *Evaluator) request(phase string, in Instance, cfg any, job *Job, d *Description, now time.Time) map[string]any {
	o := e.C.Office
	st, _ := ReadState(e.C.L, o.Name)
	var stateV any = map[string]any{}
	if st != nil {
		stateV = st
	}
	var jobV any = map[string]any{}
	if job != nil {
		jobV = map[string]any{
			"id": job.ID, "kind": job.Kind, "scope": job.Scope, "area": job.Area, "asked_by": job.AskedBy,
			"signer":   map[string]any{"principal": job.AskedBy, "region": e.C.Region.Region},
			"priority": job.Priority, "needs": job.Needs, "effects": nonNil(job.Effects), "surface": job.Surface,
			"profile": job.Profile, "repo": job.Repo, "expires": job.Expires, "signed_at": job.SignedAt,
			"tags": nonNilMap(job.Tags),
		}
	}
	history := Duration(d.History, 0)
	acc := []accepted{}
	if history > 0 {
		acc = acceptedOnMachine(e.C.L, now.Add(-history))
	}
	facts := e.Facts
	if facts == nil {
		facts = NewFacts()
	}
	officeRunning := []map[string]any{}
	for _, r := range ReadRunning(e.C.L, o.Name) {
		officeRunning = append(officeRunning, map[string]any{"id": r.ID, "kind": r.Kind, "priority": r.Priority,
			"asked_by": r.AskedBy, "started": r.Started, "held": r.State != "running"})
	}
	machineRunning := []map[string]any{}
	for _, r := range MachineRunning(e.C.L) {
		machineRunning = append(machineRunning, map[string]any{"office": r.Office, "id": r.ID, "priority": r.Priority})
	}
	return map[string]any{
		"contract":  Contract,
		"phase":     phase,
		"component": e.Component,
		"rule":      map[string]any{"name": in.Name, "config": cfg},
		"job":       jobV,
		"office": map[string]any{
			"name": o.Name, "region": o.Region, "machine": o.Machine, "owner": nonNil(o.Owner), "tier": o.Tier,
			"kinds": nonNil(o.Kinds), "holdings": o.Holdings, "state": stateV, "running": officeRunning,
		},
		"machine": map[string]any{
			"name": e.C.Machine.Machine, "running": machineRunning, "accepted": acc,
			"facts": facts.For(e, d.Facts, now),
		},
		"clock": map[string]any{"now": now.Format(time.RFC3339), "timezone": e.C.Region.Timezone},
	}
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// acceptedOnMachine is the machine's record of accepted placements since t:
// every office's accepted/ folder on this machine.
func acceptedOnMachine(l Layout, since time.Time) []accepted {
	out := []accepted{}
	dirs, _ := filepath.Glob(filepath.Join(l.Var, "*", "accepted"))
	for _, d := range dirs {
		office := filepath.Base(filepath.Dir(d))
		files, _ := os.ReadDir(d)
		for _, f := range files {
			info, err := f.Info()
			if err != nil || info.ModTime().Before(since) || !strings.HasSuffix(f.Name(), ".placement") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(d, f.Name()))
			if err != nil {
				continue
			}
			j, err := Decode(b)
			if err != nil {
				continue
			}
			out = append(out, accepted{Office: office, ID: j.ID, Priority: j.Priority, At: info.ModTime().UTC().Format(time.RFC3339)})
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].At < out[k].At })
	return out
}

// ---------------------------------------------------------------- logging

func (e *Evaluator) log(phase, job string, res *Result, now time.Time) error {
	o := e.C.Office.Name
	t := Stamp(now)
	// At the run phase the same verdicts come every interval; repeats of an
	// identical one for one job and instance collapse (spec §9).
	var st *streaks
	if phase == "run" {
		st = loadStreaks(e.C.L, o)
	}
	emit := func(key, sig string, line Entry, extra ...Entry) error {
		if st != nil {
			flush, write := st.see(key, sig, line, t)
			if flush != nil {
				if err := Append(e.C.L, o, flush); err != nil {
					return err
				}
			}
			if !write {
				return nil
			}
		}
		if err := Append(e.C.L, o, line); err != nil {
			return err
		}
		for _, x := range extra {
			if err := Append(e.C.L, o, x); err != nil {
				return err
			}
		}
		return nil
	}
	for _, r := range res.Instances {
		line := Entry{"time": t, "event": "verdict", "phase": phase, "component": e.Component, "job": job,
			"instance": r.Name, "source": r.Source, "plugin": r.Plugin, "plugin_version": r.PluginVersion,
			"sha256": r.SHA256, "contract": r.Contract, "verdict": r.Verdict.Verdict, "reason": r.Verdict.Reason,
			"retry_after": r.Verdict.RetryAfter, "note": r.Verdict.Note, "ms": r.MS, "result": res.Verdict}
		if r.StateMTime != "" {
			line["state_mtime"] = r.StateMTime
		}
		if r.Verdict.Alert != nil {
			line["alert"] = r.Verdict.Alert
		}
		var extra []Entry
		if r.Failed != "" {
			extra = append(extra, Entry{"time": t, "event": "rule-failed", "phase": phase, "component": e.Component,
				"job": job, "instance": r.Name, "plugin": r.Plugin, "what": r.Failed, "alert": Alert{"smoke", r.Verdict.Reason}})
		}
		sig := strings.Join([]string{r.Verdict.Verdict, r.Verdict.Reason, r.Verdict.RetryAfter, r.Failed, r.SHA256,
			res.Verdict}, "\x00")
		if err := emit(job+"/"+r.Name, sig, line, extra...); err != nil {
			return err
		}
	}
	line := Entry{"time": t, "event": "evaluated", "phase": phase, "component": e.Component,
		"job": job, "result": res.Verdict, "reason": res.Reason, "retry_after": res.RetryAfter, "by": res.By,
		"instances": len(res.Instances)}
	if res.OnClosed != "" {
		line["on_closed"] = res.OnClosed
	}
	if res.Override != nil {
		line["override"] = res.Override
	}
	sig := strings.Join([]string{res.Verdict, res.Reason, res.RetryAfter, res.By, res.OnClosed}, "\x00")
	if err := emit(job+"/", sig, line); err != nil {
		return err
	}
	if st != nil {
		return st.save()
	}
	return nil
}

// cache keeps the office-wide verdicts a place evaluation produced, for
// describe to publish until each one's valid_until.
func (e *Evaluator) cache(res *Result, now time.Time) {
	m := map[string]Published{}
	for _, p := range ReadPublished(e.C.L, e.C.Office.Name, now) {
		m[p.Instance] = p
	}
	for _, r := range res.Instances {
		v := r.Verdict
		if v.Holds != "office" || v.ValidUntil == "" || r.Failed != "" {
			delete(m, r.Name)
			continue
		}
		vu, err := time.Parse(time.RFC3339Nano, v.ValidUntil)
		if err != nil || !vu.After(now) {
			delete(m, r.Name)
			continue
		}
		m[r.Name] = Published{Instance: r.Name, Verdict: v.Verdict, Reason: v.Reason, RetryAfter: v.RetryAfter, ValidUntil: v.ValidUntil}
	}
	list := make([]Published, 0, len(m))
	for _, p := range m {
		list = append(list, p)
	}
	sort.Slice(list, func(i, k int) bool { return list[i].Instance < list[k].Instance })
	b, _ := json.Marshal(list)
	_ = os.MkdirAll(e.C.L.VarDir(e.C.Office.Name), 0o755)
	_ = WriteAtomic(e.C.L.VerdictCache(e.C.Office.Name), b, 0o644)
}

// ReadPublished is the office-wide verdicts still valid at now. An instance
// no longer on the office drops out with the next evaluation.
func ReadPublished(l Layout, office string, now time.Time) []Published {
	b, err := os.ReadFile(l.VerdictCache(office))
	if err != nil {
		return []Published{}
	}
	var list []Published
	if json.Unmarshal(b, &list) != nil {
		return []Published{}
	}
	out := []Published{}
	for _, p := range list {
		if vu, err := time.Parse(time.RFC3339Nano, p.ValidUntil); err == nil && vu.After(now) {
			out = append(out, p)
		}
	}
	return out
}
