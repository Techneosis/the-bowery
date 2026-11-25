package offices

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// Entry is one office-log line: an event plus its fields, appended as JSON.
type Entry map[string]any

// DecisionEvents are the lines of the decisions log: what the rules and
// the owner decided. Everything else is the office's activity.
var DecisionEvents = []string{"verdict", "evaluated", "rule-failed", "state", "state-expired", "stop-asked"}

func isDecision(e Entry) bool {
	ev, _ := e["event"].(string)
	return contains(DecisionEvents, ev)
}

// Append writes one line, never rewriting what is there. A log that can't
// be written is an error the caller must act on: every verdict is logged,
// or the evaluation doesn't count.
func Append(l Layout, office string, e Entry) error { return AppendAll(l, office, []Entry{e}) }

// AppendAll writes lines in order: activity to the office's log, decisions
// to the decisions log. In root mode decisions go through the recorder
// (`sudo -n -u <recorder> office record <office>`), so the office's user,
// which runs the pull job, can add to the record but never rewrite it.
func AppendAll(l Layout, office string, es []Entry) error {
	var act, dec [][]byte
	for _, e := range es {
		if _, ok := e["time"]; !ok {
			e["time"] = Stamp(time.Now())
		}
		e["office"] = office
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if isDecision(e) {
			dec = append(dec, b)
		} else {
			act = append(act, b)
		}
	}
	if len(act) > 0 {
		if err := appendLines(l.LogFile(office), act); err != nil {
			return err
		}
	}
	if len(dec) == 0 {
		return nil
	}
	if l.Dev {
		return appendLines(l.DecisionsFile(office), dec)
	}
	if l.Recorder == "" {
		return fmt.Errorf("root mode writes decisions through the recorder, and the machine file names none (recorder = \"…\")")
	}
	if Whoami() == l.Recorder {
		return RecordDecisions(l, office, dec)
	}
	argv := recorderArgv(l, office)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(append(bytes.Join(dec, []byte("\n")), '\n'))
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("the recorder didn't take the decisions: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func recorderArgv(l Layout, office string) []string {
	self, err := os.Executable()
	if err != nil {
		self = "office"
	}
	return []string{"sudo", "-n", "-u", l.Recorder, self, "record", office}
}

// RecordDecisions is the recorder's side: append decision lines to the
// office's decisions log. In root mode the log and every directory above it
// up to the var root must be out of the office user's reach.
func RecordDecisions(l Layout, office string, lines [][]byte) error {
	for _, b := range lines {
		e := Entry{}
		if err := json.Unmarshal(b, &e); err != nil {
			return fmt.Errorf("a line that isn't a JSON object: %v", err)
		}
		if !isDecision(e) {
			return fmt.Errorf("event %v isn't a decision", e["event"])
		}
		if o, _ := e["office"].(string); o != office {
			return fmt.Errorf("a line for office %q, not %s", o, office)
		}
	}
	p := l.DecisionsFile(office)
	if !l.Dev {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := notOfficeWritable(l, office, p); err != nil {
			return err
		}
	}
	return appendLines(p, lines)
}

// RecorderCallerOK says whether the user who reached the recorder through
// sudo (SUDO_USER) may write this office's decisions: the office's own user
// (its pull job) or one of its owners. Any other caller, such as another
// office's user the sudoers line also admits, would forge this office's
// record. "" is no sudo at all: the recorder itself.
func RecorderCallerOK(c *Config, sudoUser string) error {
	if sudoUser == "" || sudoUser == c.Office.User || contains(c.Office.Owner, sudoUser) {
		return nil
	}
	return fmt.Errorf("%s is neither %s's user nor one of its owners, so it can't record for it", sudoUser, c.Office.Name)
}

// notOfficeWritable: in root mode, path (or, before it exists, its nearest
// existing parent) and every directory above it up to the var root are owned by
// someone other than the office's user and writable by nobody else.
func notOfficeWritable(l Layout, office, path string) error {
	u, err := user.Lookup(office)
	if err != nil {
		return fmt.Errorf("there is no user %s: %v", office, err)
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	// Not there yet (no override, no decision): judge the nearest thing
	// that is, since whoever can write it could create the rest.
	for {
		if _, err := os.Lstat(path); !os.IsNotExist(err) || path == l.Var || path == "/" {
			break
		}
		path = filepath.Dir(path)
	}
	if err := chainProblem(path, l.Var, statOwner, func(o uint32) bool { return o != uint32(uid) }); err != nil {
		return fmt.Errorf("the office's user could write %s: %v", path, err)
	}
	return nil
}

func appendLines(p string, lines [][]byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var buf bytes.Buffer
	for _, b := range lines {
		buf.Write(b)
		buf.WriteByte('\n')
	}
	_, err = f.Write(buf.Bytes())
	return err
}

// ReadLog returns the entries at or after since (zero: all), from the
// activity log and the decisions log together, in time order.
func ReadLog(l Layout, office string, since time.Time) ([]Entry, error) {
	var out []Entry
	for _, p := range []string{l.LogFile(office), l.DecisionsFile(office)} {
		es, err := readLogFile(p, since)
		if err != nil {
			return nil, err
		}
		out = append(out, es...)
	}
	sort.SliceStable(out, func(i, k int) bool {
		a, _ := out[i]["time"].(string)
		b, _ := out[k]["time"].(string)
		return a < b
	})
	return out, nil
}

func readLogFile(p string, since time.Time) ([]Entry, error) {
	f, err := os.Open(p)
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
			return nil, fmt.Errorf("%s: a line that isn't JSON: %v", p, err)
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
