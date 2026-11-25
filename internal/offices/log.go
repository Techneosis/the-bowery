package offices

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Entry is one office-log line: an event plus its fields, appended as JSON.
type Entry map[string]any

// Append writes one line to the office's log, never rewriting what is there.
// A log that can't be written is an error the caller must act on: every
// verdict is logged, or the evaluation doesn't count.
func Append(l Layout, office string, e Entry) error {
	p := l.LogFile(office)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if _, ok := e["time"]; !ok {
		e["time"] = Stamp(time.Now())
	}
	e["office"] = office
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// ReadLog returns the entries at or after since (zero: all).
func ReadLog(l Layout, office string, since time.Time) ([]Entry, error) {
	f, err := os.Open(l.LogFile(office))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		e := Entry{}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s: a line that isn't JSON: %v", l.LogFile(office), err)
		}
		if !since.IsZero() {
			ts, _ := e["time"].(string)
			t, err := time.Parse(time.RFC3339Nano, ts)
			if err == nil && t.Before(since) {
				continue
			}
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Stamp is the one time format every log and record uses: UTC, millisecond
// precision, so stage timings can be read off the lines.
func Stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
