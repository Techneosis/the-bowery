package offices

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// Root mode can't run in the suite (it needs root-owned files), so its
// checks are tested here directly.

type fakeNode struct {
	uid  uint32
	mode os.FileMode
}

func fakeStat(m map[string]fakeNode) func(string) (uint32, os.FileMode, error) {
	return func(p string) (uint32, os.FileMode, error) {
		n, ok := m[p]
		if !ok {
			return 0, 0, os.ErrNotExist
		}
		return n.uid, n.mode, nil
	}
}

func rootOnly(uid uint32) bool { return uid == 0 }

func TestEveryParentUpToRootIsChecked(t *testing.T) {
	tree := map[string]fakeNode{
		"/": {0, 0o755}, "/usr": {0, 0o755}, "/usr/lib": {0, 0o755},
		"/usr/lib/rules": {0, 0o755}, "/usr/lib/rules/p": {0, 0o755},
	}
	if err := chainProblem("/usr/lib/rules/p", "/", fakeStat(tree), rootOnly); err != nil {
		t.Fatalf("a root-owned chain: %v", err)
	}
	tree["/usr/lib"] = fakeNode{1000, 0o755}
	err := chainProblem("/usr/lib/rules/p", "/", fakeStat(tree), rootOnly)
	if err == nil || !strings.Contains(err.Error(), "/usr/lib:") {
		t.Fatalf("a parent owned by a user must fail, naming it: %v", err)
	}
	tree["/usr/lib"] = fakeNode{0, 0o775}
	err = chainProblem("/usr/lib/rules/p", "/", fakeStat(tree), rootOnly)
	if err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("a group-writable parent must fail: %v", err)
	}
	tree["/usr/lib"] = fakeNode{0, 0o755}
	tree["/usr"] = fakeNode{0, 0o777 | os.ModeSticky}
	if chainProblem("/usr/lib/rules/p", "/", fakeStat(tree), rootOnly) == nil {
		t.Fatal("a sticky world-writable parent still lets others rename into it")
	}
}

func TestTheChainStopsAtTop(t *testing.T) {
	tree := map[string]fakeNode{"/var/lib/offices/owner/o/state": {1000, 0o644}, "/var/lib/offices/owner/o": {1000, 0o755},
		"/var/lib/offices/owner": {0, 0o755}, "/var/lib/offices": {0, 0o755}}
	notOffice := func(uid uint32) bool { return uid != 2000 }
	if err := chainProblem("/var/lib/offices/owner/o/state", "/var/lib/offices", fakeStat(tree), notOffice); err != nil {
		t.Fatalf("the owner's state, outside the office's reach: %v", err)
	}
	tree["/var/lib/offices/owner/o"] = fakeNode{2000, 0o755}
	if chainProblem("/var/lib/offices/owner/o/state", "/var/lib/offices", fakeStat(tree), notOffice) == nil {
		t.Fatal("a state directory the office owns lets the office replace the state")
	}
}

func me(t *testing.T) *user.User {
	u, err := user.Current()
	if err != nil {
		t.Skip("no current user")
	}
	return u
}

func TestRootModeRefusesAStateTheOfficeCouldWrite(t *testing.T) {
	u := me(t)
	l := Layout{Dev: false, Var: t.TempDir()}
	// An office whose user is the one running this test: every file it
	// made is the office's own, so its state can't be trusted.
	p := l.StateFile(u.Username)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"state":"open","by":"x","at":"2026-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadState(l, u.Username)
	if err == nil || !strings.Contains(err.Error(), "the office's user could write") {
		t.Fatalf("want a refusal, got %v", err)
	}
	// Dev mode doesn't check (and says so on every command).
	l.Dev = true
	if _, err := ReadState(l, u.Username); err != nil {
		t.Fatalf("dev mode: %v", err)
	}
}

func TestRootModeDecisionsNeedTheRecorder(t *testing.T) {
	l := Layout{Dev: false, Var: t.TempDir()}
	err := Append(l, "o", Entry{"event": "verdict"})
	if err == nil || !strings.Contains(err.Error(), "recorder") {
		t.Fatalf("a root-mode decision with no recorder must fail: %v", err)
	}
	if _, err := os.Stat(l.DecisionsFile("o")); err == nil {
		t.Fatal("it wrote the decisions log itself")
	}
	l.Recorder = "offices-rec"
	argv := recorderArgv(l, "o")
	if len(argv) < 6 || strings.Join(argv[:4], " ") != "sudo -n -u offices-rec" || argv[len(argv)-2] != "record" || argv[len(argv)-1] != "o" {
		t.Fatalf("recorder argv %q", argv)
	}
	// Activity isn't a decision: the office writes it itself.
	if err := Append(l, "o", Entry{"event": "delivered"}); err != nil {
		t.Fatalf("activity: %v", err)
	}
}

func TestRootModeSurfacesGetAnEnvironmentBuiltFromNothing(t *testing.T) {
	t.Setenv("OFFICE_ROOT", "/tmp/elsewhere")
	t.Setenv("PATH", "/evil/bin")
	t.Setenv("SECRET_TOKEN", "x")
	c := &Config{L: Layout{Dev: false}, Office: &Office{Name: "nobody-such-office", User: "nobody-such-office"}}
	env := surfaceEnv(c, "j1", "reviewer", map[string]string{"OFFICE_ON_CLOSED": "yield"})
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"OFFICE_ROOT", "/evil", "SECRET_TOKEN"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("root mode passed %s on: %q", bad, env)
		}
	}
	if !strings.Contains(joined, "PATH=/usr/local/bin:/usr/bin:/bin") || !strings.Contains(joined, "OFFICE_ON_CLOSED=yield") {
		t.Fatalf("env %q", env)
	}
	c.L = Layout{Dev: true, Etc: "/d/etc"}
	if !strings.Contains(strings.Join(surfaceEnv(c, "j1", "reviewer", nil), "\n"), "OFFICE_ROOT=/d") {
		t.Fatal("dev mode names its root")
	}
}

// The recorder is reached through sudo by every office's user and every
// owner. It must write only for the office the caller belongs to, or one
// office's user could forge the audit trail of another.
func TestTheRecorderTakesOnlyItsOfficesCallers(t *testing.T) {
	c := &Config{Office: &Office{Name: "region-a-reviews", User: "region-a-reviews", Owner: []string{"alice"}}}
	for caller, ok := range map[string]bool{
		"region-a-reviews": true,  // the office's own pull job
		"alice":            true,  // an owner's open or close
		"region-a-builder": false, // another office's user
		"mallory":          false,
		"":                 true, // not through sudo: the recorder itself, which Whoami already checked
	} {
		err := RecorderCallerOK(c, caller)
		if ok && err != nil {
			t.Errorf("%q should be taken: %v", caller, err)
		}
		if !ok && err == nil {
			t.Errorf("%q must be refused", caller)
		}
	}
}

// Two offices on one machine, in root mode. The placer is office-a's user,
// placing into builder as a signer builder trusts. Its resolver probes
// builder, and the probe can't record builder's decisions as office-a: the
// recorder rightly refuses it. So the probe must not judge for builder at
// all. It defers, and builder judges the placement when it arrives, as
// itself. Nothing of builder's is written by the placer.
func rootModeBuilder(t *testing.T) *Config {
	root := t.TempDir()
	l := Layout{Dev: false, Etc: filepath.Join(root, "etc"), Var: filepath.Join(root, "var"), Recorder: "offices-rec"}
	return &Config{L: l, Region: &Region{Region: "region-a"}, Machine: &Machine{Machine: "swarm-box", Recorder: "offices-rec"},
		Office: &Office{Name: "builder", User: "builder", Owner: []string{"alice"}, Rules: OfficeRules{Default: "open"}}}
}

func TestAnotherOfficesUserProbesWithoutJudging(t *testing.T) {
	c := rootModeBuilder(t)
	job := &Job{ID: "b-1", Kind: "builder", AskedBy: "placer", From: "region-a"}
	ans, err := Probe(NewEvaluator(c, "resolver-probe"), job, "office-a")
	if err != nil {
		t.Fatalf("a probe from another office's user: %v", err)
	}
	if ans.Verdict != "deferred" || !strings.Contains(ans.Reason, "judges it when it arrives") {
		t.Fatalf("want the probe deferred to builder, got %s: %s", ans.Verdict, ans.Reason)
	}
	if ents, _ := os.ReadDir(c.L.Var); len(ents) > 0 {
		t.Fatalf("the placer wrote builder's files: %v", ents)
	}
}

// builder's own user (and its owners) still judge at the probe: what they
// decide goes through the recorder, which takes them.
func TestTheOfficesOwnUsersStillJudgeTheProbe(t *testing.T) {
	c := rootModeBuilder(t)
	job := &Job{ID: "b-1", Kind: "builder", AskedBy: "placer", From: "region-a"}
	for _, caller := range []string{"builder", "alice"} {
		ans, _ := Probe(NewEvaluator(c, "resolver-probe"), job, caller)
		if ans.Verdict == "deferred" {
			t.Fatalf("%s's probe deferred: it must judge", caller)
		}
	}
	// An unknown caller is never taken for the office.
	if ans, _ := Probe(NewEvaluator(c, "resolver-probe"), job, ""); ans.Verdict != "deferred" {
		t.Fatalf("an unnamed caller judged: %s", ans.Verdict)
	}
}
