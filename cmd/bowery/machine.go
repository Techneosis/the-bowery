package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

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

// controlDir is where ssh's control sockets go: a short, private folder.
// A Unix socket's path is capped (104 bytes on macOS, 103 usable; 108 on
// Linux), and ssh binds a temporary name before the real one: the folder,
// "/bowery-ssh-" (12), %C's 40 hex digits, then "." and 16 random
// characters. That's the folder plus 69, so the folder may be at most
// maxControlDir bytes. $XDG_RUNTIME_DIR is used when it's that short,
// absolute and ours; else /tmp/bowery-<uid>, made 0700. Either must be a
// real folder (not a symlink), mode 0700 and owned by us. "" means none is
// safe, and ssh then runs without sharing a connection.
const maxControlDir = 103 - 69

func controlDir() string {
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && len(rt) <= maxControlDir && filepath.IsAbs(rt) && privateDir(rt) {
		return rt
	}
	d := fmt.Sprintf("/tmp/bowery-%d", os.Getuid())
	if err := os.Mkdir(d, 0o700); err != nil && !os.IsExist(err) {
		return ""
	}
	if len(d) > maxControlDir || !privateDir(d) {
		return ""
	}
	return d
}

// privateDir: a real folder (Lstat, so a symlink isn't followed), mode 0700,
// owned by this user.
func privateDir(d string) bool {
	st, err := os.Lstat(d)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(sys.Uid) == os.Getuid()
}

func sshRun(host, script string) (int, string, error) {
	args := []string{"-o", "BatchMode=yes"}
	if d := controlDir(); d != "" {
		// %C is a 40-hex hash of the connection, so the path stays short.
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPath="+filepath.Join(d, "bowery-ssh-%C"),
			"-o", "ControlPersist=60")
	}
	cmd := exec.Command("ssh", append(args, host, script)...)
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

// testAs is `test <flag> <path>` run as the agent's identity when there is
// one (its directories may be closed to anyone else), else as the login.
// Only a clear yes or no is an answer: a wrapper that fails is an error.
func testAs(flag, path string) (bool, error) {
	script := "if test " + flag + " " + shq(path) + "; then echo yes; else echo no; fi"
	var out string
	switch {
	case machine != "" && identityPrefix() != "":
		_, o, err := sshSh(identityPrefix() + "sh -c " + shq(script))
		if err != nil {
			return false, err
		}
		out = o
	case machine == "" && as != "" && cfg.IdentityCommand != "":
		b, err := exec.Command(expandUser(cfg.IdentityCommand), as, "sh", "-c", script).Output()
		if err != nil {
			return false, fail("checking %s as %s: %v", path, as, err)
		}
		out = string(b)
	default:
		if flag == "-d" {
			return isDir(path)
		}
		return exists(path)
	}
	switch strings.TrimSpace(out) {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	}
	return false, fail("checking %s as %s gave no answer: %s", path, as, strings.TrimSpace(out))
}

func isDirAs(path string) (bool, error)  { return testAs("-d", path) }
func existsAs(path string) (bool, error) { return testAs("-e", path) }

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
