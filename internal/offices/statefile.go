package offices

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"
)

// Gate is one read of a state-file plugin's file (spec §5.2): a service
// outside the office host writes a JSON object, and the core turns it into
// a verdict without running any plugin code. The verdict is the same for
// every job in every office that uses the file.
type Gate struct {
	Fields map[string]any
	MTime  time.Time
	// Fresh is the older of the file's mtime and its own timestamp: the
	// moment the file vouches for.
	Fresh time.Time
}

const maxGateFile = 64 << 10

// integrityError is a gate that can't be trusted for who could have written
// it, as opposed to one that is merely missing, stale or malformed.
type integrityError struct{ what string }

func (e *integrityError) Error() string { return e.what }

// ReadGate reads and checks a state-file plugin's file. Every way the file
// can't be trusted is an error, which the caller turns into a failed rule:
// missing, unreadable, not a regular file, not root-owned or writable by
// anyone but root (in root mode), malformed, or older than max_age by its
// mtime or its own timestamp, whichever is older.
func ReadGate(name string, p Plugin, now time.Time) (*Gate, error) {
	fi, err := os.Lstat(p.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("its state file %s is missing", p.Path)
		}
		return nil, fmt.Errorf("its state file can't be read: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("its state file %s isn't a regular file", p.Path)
	}
	if rootMode {
		// Root's, and so is every directory above it: a gate an office's
		// user could write, or swap, is no gate.
		if err := checkRootOwned(p.Path); err != nil {
			return nil, &integrityError{fmt.Sprintf("its state file isn't root's alone: %v", err)}
		}
	}
	f, err := os.Open(p.Path)
	if err != nil {
		return nil, fmt.Errorf("its state file can't be read: %v", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxGateFile+1))
	if err != nil {
		return nil, fmt.Errorf("its state file can't be read: %v", err)
	}
	if len(b) > maxGateFile {
		return nil, fmt.Errorf("its state file is over 64 KiB")
	}
	fields := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("its state file isn't a JSON object: %v", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("its state file has trailing data")
	}
	g := &Gate{Fields: fields, MTime: fi.ModTime(), Fresh: fi.ModTime()}
	if p.AtField != "" {
		at, err := gateTime(fields, p.AtField)
		if err != nil {
			return nil, err
		}
		if at.IsZero() {
			return nil, fmt.Errorf("its state file has no %s", p.AtField)
		}
		if at.Before(g.Fresh) {
			g.Fresh = at
		}
	}
	maxAge := Duration(p.MaxAge, 0)
	if maxAge <= 0 {
		return nil, fmt.Errorf("it has no max_age")
	}
	if age := now.Sub(g.Fresh); age > maxAge {
		return nil, fmt.Errorf("its state file is stale: %s old, max_age %s", age.Round(time.Second), maxAge)
	}
	return g, nil
}

// gateTime reads a timestamp field: RFC 3339, or Unix seconds. Absent or
// null is the zero time; anything else that isn't a time is malformed.
func gateTime(fields map[string]any, k string) (time.Time, error) {
	v, ok := fields[k]
	if !ok || v == nil {
		return time.Time{}, nil
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339Nano, x)
		if err != nil {
			return time.Time{}, fmt.Errorf("its state file's %s %q isn't RFC 3339", k, x)
		}
		return t, nil
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("its state file's %s isn't a time", k)
		}
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9)), nil
	}
	return time.Time{}, fmt.Errorf("its state file's %s isn't a time", k)
}

// Verdict turns a fresh gate into a verdict: open_field true is open; false
// is closed with the file's reason and retry time. Any other value of
// open_field, a reason that isn't text, or a retry time that isn't one, is
// malformed. The verdict holds for the whole office until the file would go
// stale, so describe may publish it.
func (g *Gate) Verdict(name string, p Plugin) (*Verdict, error) {
	ov, ok := g.Fields[p.OpenField]
	if !ok {
		return nil, fmt.Errorf("its state file has no %s", p.OpenField)
	}
	open, ok := ov.(bool)
	if !ok {
		return nil, fmt.Errorf("its state file's %s isn't true or false", p.OpenField)
	}
	v := &Verdict{Holds: "office", ValidUntil: g.Fresh.Add(Duration(p.MaxAge, 0)).UTC().Format(time.RFC3339Nano)}
	if open {
		v.Verdict = "open"
		return v, nil
	}
	v.Verdict = "closed"
	if p.ReasonField != "" {
		if rv, ok := g.Fields[p.ReasonField]; ok && rv != nil {
			s, ok := rv.(string)
			if !ok {
				return nil, fmt.Errorf("its state file's %s isn't text", p.ReasonField)
			}
			v.Reason = s
		}
	}
	if v.Reason == "" {
		v.Reason = name + " is closed"
	}
	if p.RetryAfterField != "" {
		t, err := gateTime(g.Fields, p.RetryAfterField)
		if err != nil {
			return nil, err
		}
		if !t.IsZero() {
			v.RetryAfter = t.UTC().Format(time.RFC3339)
		}
	}
	return v, nil
}

// runStateFile is the state-file kind's evaluation: read the file, check
// it, turn it into a verdict. Its instances take no config, and run in
// every phase unless `at` narrows them.
func (e *Evaluator) runStateFile(in Instance, p Plugin, phase string, now time.Time) *InstanceResult {
	start := time.Now()
	if len(in.Config) > 0 {
		return failed(in, "a state-file rule takes no config")
	}
	for _, ph := range in.At {
		if !contains(Phases, ph) {
			return failed(in, fmt.Sprintf("there is no phase %s", ph))
		}
	}
	if len(in.At) > 0 && !contains(in.At, phase) {
		return nil
	}
	res := &InstanceResult{Instance: in, Name: in.Name, Source: in.source, Plugin: in.Plugin, Contract: contractOf(in),
		Binding: in.Binding}
	g, err := ReadGate(in.Plugin, p, now)
	if err == nil {
		res.StateMTime = Stamp(g.MTime)
		var v *Verdict
		if v, err = g.Verdict(in.Plugin, p); err == nil {
			res.Verdict = *v
			res.MS = time.Since(start).Milliseconds()
			return res
		}
	}
	f := failed(in, err.Error())
	if _, ok := err.(*integrityError); ok {
		f = integrityFailed(in, err.Error())
	}
	f.StateMTime, f.MS = res.StateMTime, time.Since(start).Milliseconds()
	return f
}

// LivePublished is the office-wide verdicts describe publishes: those the
// last place evaluation cached, with every place-phase state-file rule read
// again now. A gate changes every few seconds, so its cached verdict would
// keep a resolver passing over an office that has since opened (or
// sending to one that has since closed). A gate that can't be trusted
// isn't published; the probe answers for it, and logs the failure.
func LivePublished(c *Config, now time.Time) []Published {
	m := map[string]Published{}
	for _, p := range ReadPublished(c.L, c.Office.Name, now) {
		m[p.Instance] = p
	}
	ev := &Evaluator{C: c}
	for _, in := range ev.Instances() {
		p, ok := c.Machine.Plugins[in.Plugin]
		if !ok || p.Kind != "state-file" {
			continue
		}
		delete(m, in.Name)
		if (len(in.At) > 0 && !contains(in.At, "place")) || len(in.Config) > 0 {
			continue
		}
		g, err := ReadGate(in.Plugin, p, now)
		if err != nil {
			continue
		}
		v, err := g.Verdict(in.Plugin, p)
		if err != nil {
			continue
		}
		m[in.Name] = Published{Instance: in.Name, Verdict: v.Verdict, Reason: v.Reason, RetryAfter: v.RetryAfter, ValidUntil: v.ValidUntil}
	}
	out := make([]Published, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Instance < out[k].Instance })
	return out
}
