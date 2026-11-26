package offices

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlacementBlockRoundTrips(t *testing.T) {
	j := &Job{ID: "j-1", Kind: "review", AskedBy: "alice", Scope: "acme", Expires: "2099-01-01T00:00:00Z",
		Needs: Needs{Tools: []string{"git", "go"}, Repo: "org/app", RepoMode: "job"}, Effects: []string{"push"},
		Tags: map[string]string{"pr": "12"}, Prompt: "line one\nline \"two\" = x", Office: "o", Machine: "m", Region: "r"}
	b := j.Encode()
	if !strings.HasPrefix(string(b), "```placement\n") {
		t.Fatalf("not a fenced block: %s", b)
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Encode()) != string(b) {
		t.Fatalf("round trip changed the bytes:\n%s\n%s", b, got.Encode())
	}
	if got.Prompt != j.Prompt || got.Needs.Tools[1] != "go" || got.Tags["pr"] != "12" {
		t.Fatalf("lost a field: %+v", got)
	}
}

func TestDecodeIsStrict(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key":   "```placement\nid = \"a\"\nrun_as = \"root\"\n```\n",
		"repeated key":  "```placement\nid = \"a\"\nid = \"b\"\n```\n",
		"unquoted":      "```placement\nid = a\n```\n",
		"no fence":      "id = \"a\"\n",
		"bad id":        "```placement\nid = \"../x\"\nkind = \"k\"\nasked_by = \"a\"\nexpires = \"2099-01-01T00:00:00Z\"\n```\n",
		"bad expires":   "```placement\nid = \"a\"\nkind = \"k\"\nasked_by = \"a\"\nexpires = \"soon\"\n```\n",
		"newline value": "```placement\nid = \"a\"\nkind = \"k\"\nasked_by = \"a\"\nexpires = \"2099-01-01T00:00:00Z\"\ntags.x = \"a\\nb\"\n```\n",
	} {
		if _, err := Decode([]byte(body)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestCombine(t *testing.T) {
	r := func(v Verdict) InstanceResult { return InstanceResult{Verdict: v} }
	res := &Result{Instances: []InstanceResult{r(Verdict{Verdict: "defer"}), r(Verdict{Verdict: "defer"})}}
	combine(res, "closed")
	if res.Verdict != "closed" || res.By != "default" {
		t.Fatalf("all defer: %+v", res)
	}
	res = &Result{Instances: []InstanceResult{r(Verdict{Verdict: "open"}), r(Verdict{Verdict: "closed", Reason: "a", RetryAfter: "2099-01-01T00:00:00Z"}),
		r(Verdict{Verdict: "closed", Reason: "b", RetryAfter: "2099-01-03T00:00:00Z"})}}
	combine(res, "open")
	if res.Verdict != "closed" || res.Reason != "a" || res.RetryAfter != "2099-01-03T00:00:00Z" {
		t.Fatalf("closed: %+v", res)
	}
}

func TestOverride(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	closedRule := InstanceResult{Verdict: Verdict{Verdict: "closed", Reason: "rule"}}
	res := &Result{Verdict: "closed", Reason: "rule", Instances: []InstanceResult{closedRule}}
	applyOverride(res, &State{State: "open"}, &Job{Kind: "k"}, now)
	if res.Verdict != "open" {
		t.Fatalf("open beats a plain rule: %+v", res)
	}
	closedRule.Binding = true
	res = &Result{Verdict: "closed", Reason: "rule", Instances: []InstanceResult{closedRule}}
	applyOverride(res, &State{State: "open"}, &Job{Kind: "k"}, now)
	if res.Verdict != "closed" {
		t.Fatalf("binding beats open: %+v", res)
	}
	res = &Result{Verdict: "open"}
	applyOverride(res, &State{State: "closed", Until: "2026-01-01T11:00:00Z"}, &Job{Kind: "k"}, now)
	if res.Verdict != "open" {
		t.Fatalf("an expired close still applies: %+v", res)
	}
}

func TestRootModeRefusesFilesNotOwnedByRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	p := filepath.Join(t.TempDir(), "region.toml")
	if err := os.WriteFile(p, []byte("region = \"r\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	devMode.Store(false)
	defer devMode.Store(false)
	if err := checkRootOwned(p); err == nil || !strings.Contains(err.Error(), "not owned by root") {
		t.Fatalf("got %v", err)
	}
	l := Layout{Etc: filepath.Dir(p), Var: t.TempDir()}
	if _, err := LoadRegion(l); err == nil {
		t.Fatal("root mode loaded a region file a user owns")
	}
	l.Dev = true
	if _, err := LoadRegion(l); err != nil {
		t.Fatalf("dev mode: %v", err)
	}
}

func TestSchemaSubset(t *testing.T) {
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []any{"w"},
		"properties": map[string]any{"w": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"n": map[string]any{"type": "integer", "minimum": 1.0}}}
	ok := map[string]any{"w": []any{"a"}, "n": 2.0}
	if err := ValidateSchema(schema, ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]any{{}, {"w": []any{1.0}}, {"w": []any{}, "x": 1.0}, {"w": []any{}, "n": 0.5}, {"w": []any{}, "n": 0.0}} {
		if ValidateSchema(schema, bad) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if ValidateSchema(map[string]any{"oneOf": []any{}}, ok) == nil {
		t.Error("an unsupported keyword was ignored")
	}
}

func TestMismatchScopeWall(t *testing.T) {
	c := &Catalog{Kinds: []string{"k"}, Holdings: Holdings{Secrets: []string{"store:acme/key"}}}
	j := &Job{Kind: "k", AskedBy: "a", Needs: Needs{Secrets: []string{"store:acme/key"}}}
	if why := Mismatch(j, c, nil); len(why) != 1 || !strings.Contains(why[0], "scope acme only") {
		t.Fatalf("got %v", why)
	}
	j.Scope = "acme"
	if why := Mismatch(j, c, nil); len(why) != 0 {
		t.Fatalf("got %v", why)
	}
}
