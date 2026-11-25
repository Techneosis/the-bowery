package offices

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Fact is one reading passed to a rule: its value and when it was read.
type Fact struct {
	Value any    `json:"value"`
	At    string `json:"at"`
}

type reading struct {
	values map[string]any
	at     time.Time
	tried  time.Time
}

// Facts is the machine's facts (spec §5.8): fact providers, run at most
// every interval, and the fields state-file plugins export. The pull loop
// keeps one for its lifetime, so a provider runs on its own interval and
// not once per evaluation; a one-shot command reads fresh. A rule receives
// only the facts it declared, and a fact older than twice its provider's
// interval (or a state file's max_age) is passed as missing: left out, for
// the rule to interpret.
type Facts struct {
	mu   sync.Mutex
	read map[string]*reading // by provider
}

func NewFacts() *Facts { return &Facts{read: map[string]*reading{}} }

// For returns the declared facts that are present and fresh at now.
func (fs *Facts) For(e *Evaluator, names []string, now time.Time) map[string]Fact {
	out := map[string]Fact{}
	if len(names) == 0 {
		return out
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	m := e.C.Machine
	for _, name := range names {
		if pn, field, ok := stateFileFact(m, name); ok {
			g, err := ReadGate(pn, m.Plugins[pn], now)
			if err != nil {
				continue // a stale or broken gate exports nothing
			}
			if v, ok := g.Fields[field]; ok && v != nil {
				out[name] = Fact{Value: v, At: Stamp(g.Fresh)}
			}
			continue
		}
		pn, fp, ok := providerOf(m, name)
		if !ok {
			continue
		}
		iv := Duration(fp.Interval, time.Minute)
		r := fs.read[pn]
		if r == nil || (now.Sub(r.at) >= iv && now.Sub(r.tried) >= iv) {
			if r == nil {
				r = &reading{}
				fs.read[pn] = r
			}
			r.tried = now
			vals, err := e.runProvider(pn, fp)
			if err != nil {
				_ = Append(e.C.L, e.C.Office.Name, Entry{"event": "fact-failed", "provider": pn, "what": err.Error(),
					"alert": Alert{"smoke", "fact provider " + pn + " failed: " + err.Error()}})
			} else {
				r.values, r.at = vals, now
			}
		}
		if r.values == nil || now.Sub(r.at) > 2*iv {
			continue
		}
		if v, ok := r.values[name]; ok {
			out[name] = Fact{Value: v, At: Stamp(r.at)}
		}
	}
	return out
}

func stateFileFact(m *Machine, fact string) (plugin, field string, ok bool) {
	names := make([]string, 0, len(m.Plugins))
	for n := range m.Plugins {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := m.Plugins[n]
		if p.Kind != "state-file" {
			continue
		}
		if f, ok := p.Facts[fact]; ok {
			return n, f, true
		}
	}
	return "", "", false
}

func providerOf(m *Machine, fact string) (string, FactProvider, bool) {
	for n, fp := range m.Facts {
		if contains(fp.Facts, fact) {
			return n, fp, true
		}
	}
	return "", FactProvider{}, false
}

// FactSource says where a fact comes from, or "" if nothing on the machine
// supplies it (for `office check`).
func FactSource(m *Machine, fact string) string {
	if p, _, ok := stateFileFact(m, fact); ok {
		return "state file " + p
	}
	if p, _, ok := providerOf(m, fact); ok {
		return "fact provider " + p
	}
	return ""
}

// runProvider runs one fact provider the way a plugin runs: pinned, from an
// empty environment, in /, capped, killed at the rule timeout. It prints
// one JSON object of the facts it declared; any other name is an error.
func (e *Evaluator) runProvider(name string, fp FactProvider) (map[string]any, error) {
	pf, _, err := openPinned(fp.Path, fp.SHA256)
	if err != nil {
		return nil, err
	}
	defer pf.Close()
	ctx, cancel := context.WithTimeout(context.Background(), Duration(e.C.Region.Defaults.RuleTimeout, 2*time.Second))
	defer cancel()
	out, err := runPlugin(ctx, pf, e.C.L.RuleState("facts-"+name), []byte("{}"), 0)
	if err != nil {
		return nil, err
	}
	vals := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(out))
	if err := dec.Decode(&vals); err != nil {
		return nil, fmt.Errorf("its output isn't a JSON object: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("its output has trailing data")
	}
	for k := range vals {
		if !contains(fp.Facts, k) {
			return nil, fmt.Errorf("it supplied %s, which it doesn't declare", k)
		}
	}
	return vals, nil
}
