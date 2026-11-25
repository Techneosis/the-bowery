package offices

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A signers line may name several namespaces, as ssh-keygen's allowed_signers
// does: namespaces="placement,git". The commas inside the quotes are the
// value's, not the option list's.
func TestSignersLineWithSeveralNamespaces(t *testing.T) {
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKaHmQeLpyvJ1hs0F0MjwKRTy"
	got := parseSigners("signers", []string{`alice namespaces="placement,git" ` + key + ` comment`})
	if len(got) != 1 || got[0].Problem != "" {
		t.Fatalf("a quoted, comma-joined namespaces value was refused: %+v", got)
	}
	if strings.Join(got[0].Namespaces, "|") != "placement|git" {
		t.Fatalf("namespaces = %q", got[0].Namespaces)
	}
	// An option still unsupported stays a problem, so the line never verifies.
	got = parseSigners("signers", []string{`alice namespaces="placement",valid-before="20300101" ` + key})
	if got[0].Problem == "" {
		t.Fatal("an unsupported option next to a namespaces one must still disable the line")
	}
}

// A FIFO (or any non-file) dropped in a box as `x.placement` must not hang
// the pull job, which holds the office's pull lock while it reads.
func TestAnInboxIgnoresAFifo(t *testing.T) {
	box := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(box, "wedge.placement"), 0o644); err != nil {
		t.Skip("no mkfifo")
	}
	if err := os.WriteFile(filepath.Join(box, "real.placement"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan []Letter, 1)
	go func() { ls, _ := (&DirInbox{Dir: box}).List(); done <- ls }()
	select {
	case ls := <-done:
		if len(ls) != 1 || ls[0].ID != "real" {
			t.Fatalf("listed %+v", ls)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("List hangs on a FIFO in the box")
	}
}

func TestCatalogsIgnoreAFifo(t *testing.T) {
	l := Layout{Var: t.TempDir()}
	dir := filepath.Join(l.Var, "catalogs", "region-b")
	_ = os.MkdirAll(dir, 0o755)
	if err := syscall.Mkfifo(filepath.Join(dir, "m1.catalog"), 0o644); err != nil {
		t.Skip("no mkfifo")
	}
	done := make(chan struct{})
	go func() { _, _ = (&DirTransport{L: l}).Catalogs("region-b", "", ""); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Catalogs hangs on a FIFO")
	}
}

// What the pull job accepts is the bytes it verified, not whatever sits at the
// path by the time it files them: a letter swapped in between verifying and
// accepting must not become the accepted placement (it is what a held job's
// peer link is later re-read from).
func TestAcceptingFilesTheVerifiedBytes(t *testing.T) {
	box, acc := t.TempDir(), filepath.Join(t.TempDir(), "accepted")
	p := filepath.Join(box, "j1.placement")
	_ = os.WriteFile(p, []byte("verified"), 0o644)
	in := &DirInbox{Dir: box}
	ls, err := in.List()
	if err != nil || len(ls) != 1 {
		t.Fatalf("%v %v", ls, err)
	}
	_ = os.WriteFile(p, []byte("swapped in after the check"), 0o644)
	if err := in.Done(ls[0], acc); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(acc, "j1.placement"))
	if string(b) != "verified" {
		t.Fatalf("the accepted letter is %q", b)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the letter is still in the box")
	}
}

// Spec §5.6: the peer's rules run first for a peer's placement, so a peer
// rule's reason and on_closed are the ones shown when it closes.
func TestPeerRulesRunFirst(t *testing.T) {
	c := &Config{Region: &Region{Region: "a", Defaults: RegionDefaults{Rules: []Instance{{Name: "region-rule"}}},
		Peers: map[string]Peer{"b": {Rules: []Instance{{Name: "peer-rule"}}}}},
		Machine: &Machine{Rules: []Instance{{Name: "machine-rule"}}},
		Office:  &Office{Rules: OfficeRules{Use: []Instance{{Name: "office-rule"}}}}}
	var names []string
	for _, in := range NewEvaluator(c, "accept").InstancesFor(&Job{From: "b"}) {
		names = append(names, in.Name)
	}
	if got := strings.Join(names, ","); got != "peer-rule,region-rule,machine-rule,office-rule" {
		t.Fatalf("order = %s", got)
	}
}
