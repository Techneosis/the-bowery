package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Every herdr call goes through the CLI, which `--machine <label>` forwards
// to a saved machine's server. The one exception is workspace.move, a socket
// method the CLI does not expose (and so cannot forward): that one is local.

type HerdrError struct {
	Code    string
	Message string
	Raw     string
}

func (e *HerdrError) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Raw
}

// Refused is a check saying no. Nothing was changed.
type Refused struct{ msg string }

func (e *Refused) Error() string { return e.msg }

func refuse(format string, args ...any) *Refused { return &Refused{fmt.Sprintf(format, args...)} }

// Failure is anything else that stops a command: ssh to a box, git, a start
// that did not take.
type Failure struct{ msg string }

func (e *Failure) Error() string { return e.msg }

func fail(format string, args ...any) *Failure { return &Failure{fmt.Sprintf(format, args...)} }

type WorktreeInfo struct {
	RepoKey          string `json:"repo_key"`
	RepoName         string `json:"repo_name"`
	IsLinkedWorktree bool   `json:"is_linked_worktree"`
	CheckoutPath     string `json:"checkout_path"`
}

type Workspace struct {
	WorkspaceID string        `json:"workspace_id"`
	Label       string        `json:"label"`
	TabCount    int           `json:"tab_count"`
	PaneCount   int           `json:"pane_count"`
	Worktree    *WorktreeInfo `json:"worktree"`
}

type Tab struct {
	TabID       string `json:"tab_id"`
	Label       string `json:"label"`
	WorkspaceID string `json:"workspace_id"`
}

type Pane struct {
	PaneID        string `json:"pane_id"`
	WorkspaceID   string `json:"workspace_id"`
	TabID         string `json:"tab_id"`
	TerminalID    string `json:"terminal_id"`
	Cwd           string `json:"cwd"`
	ForegroundCwd string `json:"foreground_cwd"`
	Agent         string `json:"agent"`
	AgentStatus   string `json:"agent_status"`
	Title         string `json:"terminal_title_stripped"`
}

type Agent struct {
	Name        string            `json:"name"`
	PaneID      string            `json:"pane_id"`
	Kind        string            `json:"agent"`
	AgentStatus string            `json:"agent_status"`
	Tokens      map[string]string `json:"tokens"`
	Cwd         string            `json:"cwd"`
	Title       string            `json:"terminal_title_stripped"`
	TerminalID  string            `json:"terminal_id"`
	Session     *AgentSession     `json:"agent_session"`
}

// AgentSession is the agent's own conversation id, as herdr reports it (for
// claude, the id `claude --resume` takes).
type AgentSession struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

func (a Agent) SessionID() string {
	if a.Session == nil || a.Session.Kind != "id" {
		return ""
	}
	return a.Session.Value
}

// herdrRaw runs a herdr CLI command against the current server and returns
// its .result, undecoded (or the plain output, for commands that print text).
func herdrRaw(args ...string) (json.RawMessage, error) {
	if machine != "" {
		args = append([]string{"--machine", machine}, args...)
	}
	cmd := exec.Command("herdr", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			return nil, &HerdrError{Message: err.Error(), Raw: "herdr: " + err.Error()}
		}
		raw := strings.TrimSpace(stderr.String())
		if raw == "" {
			raw = strings.TrimSpace(stdout.String())
		}
		var e struct {
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(raw), &e) == nil && e.Error != nil {
			return nil, &HerdrError{Code: e.Error.Code, Message: e.Error.Message, Raw: raw}
		}
		return nil, &HerdrError{Message: raw, Raw: raw}
	}
	var r struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal([]byte(stdout.String()), &r) == nil && r.Result != nil {
		return r.Result, nil
	}
	b, _ := json.Marshal(stdout.String())
	return b, nil
}

// herdr runs a command and decodes its result into out (nil: discard it).
func herdr(out any, args ...string) error {
	raw, err := herdrRaw(args...)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &HerdrError{Message: fmt.Sprintf("unexpected reply to herdr %s: %v", strings.Join(args, " "), err), Raw: string(raw)}
	}
	return nil
}

// herdrText is a command's output as text: a string result as is, anything
// else as its JSON.
func herdrText(args ...string) (string, error) {
	raw, err := herdrRaw(args...)
	if err != nil {
		return "", err
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	return string(raw), nil
}

func agents() ([]Agent, error) {
	var r struct{ Agents []Agent }
	err := herdr(&r, "agent", "list")
	return r.Agents, err
}

func panes() ([]Pane, error) {
	var r struct{ Panes []Pane }
	err := herdr(&r, "pane", "list")
	return r.Panes, err
}

func workspaces() ([]Workspace, error) {
	var r struct{ Workspaces []Workspace }
	err := herdr(&r, "workspace", "list")
	return r.Workspaces, err
}

func paneGet(id string) (Pane, error) {
	var r struct{ Pane Pane }
	err := herdr(&r, "pane", "get", id)
	return r.Pane, err
}

func socketPath() string {
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	return filepath.Join(home(), ".config", "herdr", "herdr.sock")
}

// rpc is a raw socket request, for methods the CLI doesn't expose.
func rpc(method string, params any) error {
	conn, err := net.DialTimeout("unix", socketPath(), 15*time.Second)
	if err != nil {
		return &HerdrError{Message: method + ": " + err.Error(), Raw: err.Error()}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	req, _ := json.Marshal(map[string]any{"id": "bowery:" + method, "method": method, "params": params})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return &HerdrError{Message: method + ": " + err.Error(), Raw: err.Error()}
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return &HerdrError{Message: method + ": " + err.Error(), Raw: err.Error()}
	}
	var resp struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &resp) == nil && resp.Error != nil {
		return &HerdrError{Code: resp.Error.Code, Message: resp.Error.Message, Raw: string(line)}
	}
	return nil
}

func findSwarm(all []Workspace) *Workspace {
	for i := range all {
		if all[i].Label == cfg.Swarm && all[i].Worktree == nil {
			return &all[i]
		}
	}
	return nil
}

// swarmLast puts swarm at the bottom of the workspace list; a no-op when it is
// there. Only ever moves the swarm workspace. insert_index counts positions
// before the move, so "last" is len(list). Local only.
func swarmLast() error {
	if machine != "" {
		return nil
	}
	all, err := workspaces()
	if err != nil {
		return err
	}
	sw := findSwarm(all)
	if sw == nil || all[len(all)-1].WorkspaceID == sw.WorkspaceID {
		return nil
	}
	return rpc("workspace.move", map[string]any{"workspace_id": sw.WorkspaceID, "insert_index": len(all)})
}

// callerPane is the pane this command runs in, when it is a pane of the
// server we talk to.
func callerPane() string {
	if machine != "" {
		return ""
	}
	id := os.Getenv("HERDR_PANE_ID")
	sock := os.Getenv("HERDR_SOCKET_PATH")
	if sock == "" {
		sock = socketPath()
	}
	if id != "" && sock == socketPath() {
		return id
	}
	return ""
}

func resolvePane(target string) (string, error) {
	ags, err := agents()
	if err != nil {
		return "", err
	}
	for _, a := range ags {
		if a.Name == target || a.PaneID == target {
			return a.PaneID, nil
		}
	}
	if strings.Contains(target, ":") {
		return target, nil
	}
	return "", nil
}

func setSound(pane, mode string) error {
	if mode == "off" {
		return herdr(nil, "pane", "report-metadata", pane, "--source", cfg.SoundSource, "--token", "sound=off")
	}
	return herdr(nil, "pane", "report-metadata", pane, "--source", cfg.SoundSource, "--clear-token", "sound")
}

// paneIsBlankShell is true only when the pane provably sits at an idle shell
// prompt: herdr says its foreground job is the shell itself and nothing else.
func paneIsBlankShell(p Pane) bool {
	blank, err := paneBlankShell(p)
	return err == nil && blank
}

// paneBlankShell is paneIsBlankShell that says when it can't tell.
func paneBlankShell(p Pane) (bool, error) {
	if p.Agent != "" {
		return false, nil
	}
	var r struct {
		ProcessInfo struct {
			ShellPID  int `json:"shell_pid"`
			FgPGID    int `json:"foreground_process_group_id"`
			Processes []struct {
				PID int `json:"pid"`
			} `json:"foreground_processes"`
		} `json:"process_info"`
	}
	if err := herdr(&r, "pane", "process-info", "--pane", p.PaneID); err != nil {
		return false, err
	}
	pi := r.ProcessInfo
	if pi.ShellPID == 0 || pi.FgPGID != pi.ShellPID {
		return false, nil
	}
	for _, pr := range pi.Processes {
		if pr.PID != pi.ShellPID {
			return false, nil
		}
	}
	return true, nil
}

func groupOf(ws Workspace, all []Workspace) []Workspace {
	if ws.Worktree == nil || ws.Worktree.RepoKey == "" {
		return nil
	}
	var out []Workspace
	for _, w := range all {
		if w.Worktree != nil && w.Worktree.RepoKey == ws.Worktree.RepoKey && w.WorkspaceID != ws.WorkspaceID {
			out = append(out, w)
		}
	}
	return out
}

func isPrimary(ws Workspace) bool { return ws.Worktree != nil && !ws.Worktree.IsLinkedWorktree }

func linkedSiblings(ws Workspace, all []Workspace) []Workspace {
	var out []Workspace
	for _, w := range groupOf(ws, all) {
		if w.Worktree.IsLinkedWorktree {
			out = append(out, w)
		}
	}
	return out
}

func labels(ws []Workspace) string {
	l := make([]string, len(ws))
	for i, w := range ws {
		l[i] = w.Label
	}
	return strings.Join(l, ", ")
}

// safeClosePane closes a pane without ever taking a sibling worktree
// workspace with it. `pane close` on the last pane of a group primary closes
// the whole group, so a last pane goes by `workspace close`, which the server
// refuses for a primary that still has linked workspaces.
func safeClosePane(paneID string, dryRun bool) (string, error) {
	all, err := workspaces()
	if err != nil {
		return "", err
	}
	ps, err := panes()
	if err != nil {
		return "", err
	}
	var pane *Pane
	for i := range ps {
		if ps[i].PaneID == paneID {
			pane = &ps[i]
		}
	}
	if pane == nil {
		return "already gone", nil
	}
	var ws Workspace
	for _, w := range all {
		if w.WorkspaceID == pane.WorkspaceID {
			ws = w
		}
	}
	if ws.PaneCount > 1 {
		if !dryRun {
			if err := herdr(nil, "pane", "close", paneID); err != nil {
				return "", err
			}
		}
		return "closed pane " + paneID, nil
	}
	if isPrimary(ws) {
		if linked := linkedSiblings(ws, all); len(linked) > 0 {
			return "", refuse("%s is the last pane of %s (%s), the herdr group primary of %d open worktree workspace(s) (%s); "+
				"closing it would close them too. Tear those down first.", paneID, ws.WorkspaceID, ws.Label, len(linked), labels(linked))
		}
	}
	if !dryRun {
		if err := herdr(nil, "workspace", "close", ws.WorkspaceID); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("closed pane %s and its workspace %s (%s)", paneID, ws.WorkspaceID, ws.Label), nil
}
