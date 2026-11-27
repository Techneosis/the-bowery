package offices

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// A launch that fails leaves no running job, so nothing else ever says it
// happened: the office's log has a `launched` line with its exit, and the
// job is gone. pull says so on the bus too, as a job.failed event from the
// office's own seat, so whoever follows job.* hears of it at once.

// JobFailedKind is the bus topic of a launch that failed.
const JobFailedKind = "job.failed"

// stderrTailMax bounds what is read back of a surface's stderr.
const stderrTailMax = 4 << 10

// busCallTimeout bounds one publish to the local node: a pull loop never
// waits on a stuck node for longer than this.
const busCallTimeout = 5 * time.Second

// stderrFile is a surface's stderr: an unlinked temporary file, so a
// process the surface leaves behind can keep writing to it without holding
// up the surface's exit (a pipe would). Nil when one can't be made; the
// surface's stderr is then discarded, as it was before.
func stderrFile() *os.File {
	f, err := os.CreateTemp("", "office-surface-stderr-")
	if err != nil {
		return nil
	}
	_ = os.Remove(f.Name())
	return f
}

// lastLine is the last non-empty line of what f holds, from its last 4 KiB.
func lastLine(f *os.File) string {
	if f == nil {
		return ""
	}
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := st.Size() - stderrTailMax
	if off < 0 {
		off = 0
	}
	b := make([]byte, st.Size()-off)
	n, _ := f.ReadAt(b, off)
	lines := strings.Split(strings.TrimRight(string(b[:n]), "\r\n\t "), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			if len(s) > 500 {
				s = s[:500]
			}
			return s
		}
	}
	return ""
}

// publishFailed puts one job.failed event on the region's local bus node,
// as the office's seat, and logs either its record or why it didn't go
// (no node, a seat that may not publish it): a failed publish never stops
// the pull.
func publishFailed(c *Config, j *Job, surface string, exit *int, stderr, why string) error {
	data := map[string]any{"surface": surface}
	if exit != nil {
		data["exit"] = *exit
	}
	if stderr != "" {
		data["stderr"] = stderr
	}
	if why != "" {
		data["why"] = why
	}
	subject := map[string]any{"job": j.ID, "office": c.Office.Name, "kind": j.Kind}
	ref, err := busPublish(c, JobFailedKind, subject, data)
	if err != nil {
		return Append(c.L, c.Office.Name, Entry{"event": "failed-not-published", "job": j.ID, "why": err.Error()})
	}
	return Append(c.L, c.Office.Name, Entry{"event": "failed-published", "job": j.ID, "ref": ref})
}

// busPublish is one publish request on the node's unix socket (the wire of
// pkg/bus's Request and Reply). In root mode the socket's peer, the
// office's user, is the seat; in dev mode the request names it.
func busPublish(c *Config, kind string, subject, data any) (string, error) {
	sb, _ := json.Marshal(subject)
	db, _ := json.Marshal(data)
	req := map[string]any{"op": "publish", "publication": map[string]any{
		"kind": kind, "subject": json.RawMessage(sb), "data": json.RawMessage(db)}}
	if c.L.Dev {
		req["as"] = c.Office.User
	}
	sock := c.L.BusSocket()
	conn, err := net.DialTimeout("unix", sock, busCallTimeout)
	if err != nil {
		return "", fmt.Errorf("no bus node at %s: %v", sock, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(busCallTimeout))
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return "", fmt.Errorf("the bus node: %v", err)
	}
	line, err := bufio.NewReaderSize(conn, 128<<10).ReadBytes('\n')
	if err != nil {
		return "", fmt.Errorf("the bus node gave no answer: %v", err)
	}
	var r struct {
		Error string `json:"error"`
		Ref   *struct {
			Producer string `json:"producer"`
			Seq      uint64 `json:"seq"`
		} `json:"ref"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return "", fmt.Errorf("the bus node's answer isn't a reply: %v", err)
	}
	if r.Error != "" {
		return "", errors.New(r.Error)
	}
	if r.Ref == nil {
		return "", errors.New("the bus node's answer names no record")
	}
	return fmt.Sprintf("%s:%d", r.Ref.Producer, r.Ref.Seq), nil
}
