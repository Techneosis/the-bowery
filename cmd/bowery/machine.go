package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// The server this run talks to. "" is the local one; a label routes every
// herdr call through `herdr --machine <label>` and every git/filesystem call
// through ssh. Set by useMachine, from --machine or from a dispatch record.
var (
	machine string
	as      string // who the agent and its git work run as on the box
	remotes = map[string]*remoteFacts{}
)

type remoteFacts struct {
	host   string // ssh destination
	home   string // $HOME on the box
	wtroot string // herdr worktrees dir on the box
}

var nameRE = regexp.MustCompile(`^[a-z0-9_-]+$`)

func useMachine(label, who string) {
	machine = label
	as = who
}

type machineProfile struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Target  string `json:"target"`
	Enabled *bool  `json:"enabled"`
}

func lookupProfile(label string) (machineProfile, error) {
	var profiles []machineProfile
	out, err := exec.Command("herdr", "machine", "list", "--json").Output()
	if err == nil {
		_ = json.Unmarshal(out, &profiles)
	}
	for _, m := range profiles {
		if m.Label == label || m.ID == label {
			if m.Enabled != nil && !*m.Enabled {
				return m, fail("the herdr machine profile %s is disabled", label)
			}
			return m, nil
		}
	}
	return machineProfile{}, fail("no saved herdr machine labelled %s (`herdr machine list`)", label)
}

// remote is the current machine's box, read once: ssh host, $HOME, and the
// herdr worktrees dir from the box's own herdr config.
func remote() (*remoteFacts, error) {
	if r, ok := remotes[machine]; ok {
		return r, nil
	}
	host := machineConfig(machine).SSH
	if host == "" {
		prof, err := lookupProfile(machine)
		if err != nil {
			return nil, err
		}
		host = prof.Target
		if host == "" {
			host = machine
		}
	}
	rc, out, err := sshRun(host, `printf "%s\n" "$HOME"; cat ~/.config/herdr/config.toml 2>/dev/null; true`)
	if err != nil {
		return nil, err
	}
	if rc != 0 || !strings.HasPrefix(out, "/") {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = "no output"
		}
		return nil, fail("ssh %s failed: %s", host, msg)
	}
	h, rest, _ := strings.Cut(out, "\n")
	r := &remoteFacts{host: host, home: strings.TrimSpace(h)}
	r.wtroot = herdrWorktreeDir(rest, r.home)
	remotes[machine] = r
	return r, nil
}

// herdrWorktreeDir reads [worktrees] directory from a herdr config.toml's
// text, ~ being home; herdr's default when it says nothing.
func herdrWorktreeDir(text, homeDir string) string {
	var c struct {
		Worktrees struct {
			Directory string `toml:"directory"`
		} `toml:"worktrees"`
	}
	d := ""
	if _, err := toml.Decode(text, &c); err == nil {
		d = c.Worktrees.Directory
	}
	if d == "~" || strings.HasPrefix(d, "~/") {
		d = homeDir + d[1:]
	}
	if d == "" {
		d = homeDir + "/.herdr/worktrees"
	}
	return d
}

func sshRun(host, script string) (int, string, error) {
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = os.TempDir()
	}
	ctl := filepath.Join(rt, "bowery-ssh-%C")
	cmd := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "ControlMaster=auto", "-o", "ControlPath="+ctl,
		"-o", "ControlPersist=60", host, script)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	rc := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			return 0, "", fail("ssh %s: %v", host, err)
		}
		rc = ee.ExitCode()
	}
	// 255 is ssh itself failing, 126/127 a missing tool on the box: never let
	// either pass for an answer ("not a worktree", "not merged"), or done would
	// take a live checkout for one that is already gone.
	if rc == 126 || rc == 127 || rc == 255 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return rc, "", fail("ssh %s failed (exit %d): %s", host, rc, msg)
	}
	if rc == 0 {
		return 0, stdout.String(), nil
	}
	if stderr.Len() > 0 {
		return rc, stderr.String(), nil
	}
	return rc, stdout.String(), nil
}

// sshSh runs a shell snippet on the current machine's box.
func sshSh(script string) (int, string, error) {
	r, err := remote()
	if err != nil {
		return 0, "", err
	}
	return sshRun(r.host, script)
}

var safeShell = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

// shq quotes one word for a POSIX shell, as Python's shlex.quote does.
func shq(s string) string {
	if s == "" {
		return "''"
	}
	if safeShell.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func shJoin(args ...string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shq(a)
	}
	return strings.Join(q, " ")
}

// remotePath is a path on the box. ~ and this machine's own $HOME (which a
// shell expands before bowery sees it) map to the box's home, so
// `--cwd ~/src/app` means the box's ~/src/app.
func remotePath(p string) (string, error) {
	r, err := remote()
	if err != nil {
		return "", err
	}
	h := home()
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		return r.home + p[1:], nil
	case p == h || strings.HasPrefix(p, h+"/"):
		return r.home + p[len(h):], nil
	case !strings.HasPrefix(p, "/"):
		return "", fail("%s: give a path on %s (absolute or ~/...), not one relative to here", p, machine)
	}
	return filepath.Clean(p), nil
}

// hostPath turns a path the user typed into one on the current server.
func hostPath(p string) (string, error) {
	if machine != "" {
		return remotePath(p)
	}
	return absPath(expandUser(p)), nil
}

func rTest(flag, path string) (bool, error) {
	rc, _, err := sshSh("test " + flag + " " + shq(path))
	return rc == 0, err
}

func isDir(path string) (bool, error) {
	if machine != "" {
		return rTest("-d", path)
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir(), nil
}

func exists(path string) (bool, error) {
	if machine != "" {
		return rTest("-e", path)
	}
	_, err := os.Lstat(path)
	return err == nil, nil
}

// norm is a comparable form of a path on the current server: symlinks
// resolved locally (as far as the path exists), only cleaned on a box.
func norm(path string) string {
	if machine != "" {
		return filepath.Clean(path)
	}
	return resolvePath(path)
}

func resolvePath(p string) string {
	p = absPath(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func under(path, root string) bool {
	rel, err := filepath.Rel(norm(root), norm(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// identityPrefix is what runs a remote command as `as`, or "" with no one.
func identityPrefix() string {
	if machine == "" || as == "" || machineConfig(machine).IdentityCommand == "" {
		return ""
	}
	return machineConfig(machine).IdentityCommand + " " + shq(as) + " "
}
