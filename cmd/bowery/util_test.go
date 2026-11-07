package main

import "testing"

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
	// The longest control socket path ssh will make there: the dir, the
	// name, and %C's 40 hex digits; it must fit macOS's 104 bytes.
	fits := func(d string) bool { return len(d)+len("/bowery-ssh-")+40 < 104 }
	t.Setenv("XDG_RUNTIME_DIR", "/private/tmp/claude-502/-Users-someone-dev-a-long-project-path/0123456789abcdef/scratchpad")
	if d := controlDir(); d == "" || !fits(d) {
		t.Errorf("with a long XDG_RUNTIME_DIR, controlDir() = %q", d)
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if d := controlDir(); d != "/run/user/1000" {
		t.Errorf("a short XDG_RUNTIME_DIR is used as it is, got %q", d)
	}
}
