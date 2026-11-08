package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShq(t *testing.T) {
	for in, want := range map[string]string{
		"":            "''",
		"plain/path":  "plain/path",
		"two words":   "'two words'",
		"it's":        `'it'"'"'s'`,
		"$HOME":       "'$HOME'",
		"a@b:c,d=e+f": "a@b:c,d=e+f",
	} {
		if got := shq(in); got != want {
			t.Errorf("shq(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestJSONLineMatchesPythonsSpacing(t *testing.T) {
	got := jsonLine([]kv{{"name", "a"}, {"worktree", `/x/"q"`}})
	want := `{"name": "a", "worktree": "/x/\"q\""}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestHerdrWorktreeDir(t *testing.T) {
	cases := []struct{ cfg, want string }{
		{"", "/h/.herdr/worktrees"},
		{"[worktrees]\ndirectory = \"~/wt\"\n", "/h/wt"},
		{"[worktrees]\ndirectory = \"/srv/wt\"\n", "/srv/wt"},
		{"not = [toml", "/h/.herdr/worktrees"},
	}
	for _, c := range cases {
		if got := herdrWorktreeDir(c.cfg, "/h"); got != c.want {
			t.Errorf("herdrWorktreeDir(%q) = %s, want %s", c.cfg, got, c.want)
		}
	}
}

func TestUnder(t *testing.T) {
	machine = "box" // plain path comparison, no symlinks
	defer func() { machine = "" }()
	for _, c := range []struct {
		p, root string
		want    bool
	}{
		{"/a/b", "/a", true}, {"/a", "/a", true}, {"/ab", "/a", false}, {"/a/../b", "/a", false},
	} {
		if got := under(c.p, c.root); got != c.want {
			t.Errorf("under(%s, %s) = %v", c.p, c.root, got)
		}
	}
}

func TestControlDirIsShortEnoughForASocket(t *testing.T) {
	// The longest path ssh binds there: the folder, "/bowery-ssh-", %C's 40
	// hex, then ssh's ".<16 random>" temporary suffix. It must fit macOS's
	// 104 bytes (103 usable).
	fits := func(d string) bool { return len(d)+len("/bowery-ssh-")+40+17 <= 103 }
	if !fits(strings.Repeat("x", maxControlDir)) || fits(strings.Repeat("x", maxControlDir+1)) {
		t.Fatalf("maxControlDir %d is not the boundary", maxControlDir)
	}
	private := func() string {
		d := t.TempDir()
		short := filepath.Join("/tmp", filepath.Base(d))
		if err := os.Symlink(d, short); err != nil {
			t.Skip("can't make a short folder in /tmp")
		}
		t.Cleanup(func() { os.Remove(short) })
		real := short + "-real"
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Skip("can't make a short folder in /tmp")
		}
		t.Cleanup(func() { os.Remove(real) })
		return real
	}
	d := private()
	t.Setenv("XDG_RUNTIME_DIR", d)
	if got := controlDir(); got != d {
		t.Errorf("a short private XDG_RUNTIME_DIR is used, got %q", got)
	}
	os.Chmod(d, 0o755)
	if got := controlDir(); got == d {
		t.Errorf("a folder others can read isn't used")
	}
	os.Chmod(d, 0o700)
	link := d + "-link"
	os.Symlink(d, link)
	t.Cleanup(func() { os.Remove(link) })
	t.Setenv("XDG_RUNTIME_DIR", link)
	if got := controlDir(); got == link {
		t.Errorf("a symlink isn't used")
	}
	t.Setenv("XDG_RUNTIME_DIR", "relative/dir")
	if got := controlDir(); got == "relative/dir" {
		t.Errorf("a relative folder isn't used")
	}
	t.Setenv("XDG_RUNTIME_DIR", "/private/tmp/claude-502/-Users-someone-dev-a-long-project-path/scratchpad")
	if got := controlDir(); got != "" && !fits(got) {
		t.Errorf("with a long XDG_RUNTIME_DIR, controlDir() = %q, too long", got)
	}
}
