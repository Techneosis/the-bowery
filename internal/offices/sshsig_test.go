package offices

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func sshKeygen(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	return p
}

func newKey(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	out, err := exec.Command(sshKeygen(t), "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	return path
}

// Our signatures verify with ssh-keygen, and ssh-keygen's verify with ours:
// the format is the one `ssh-keygen -Y` reads and writes.
func TestSignaturesInteroperateWithSSHKeygen(t *testing.T) {
	dir := t.TempDir()
	path := newKey(t, dir, "alice")
	k, err := LoadPrivateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	pubLine, _ := os.ReadFile(path + ".pub")
	pub, err := ParsePublicKey(string(pubLine))
	if err != nil || !pub.Equal(k.Public) {
		t.Fatalf("public halves differ: %v", err)
	}
	msg := []byte("```placement\nid = \"j1\"\n```\n")

	sig := k.Sign(NSPlacement, msg)
	signers := filepath.Join(dir, "allowed")
	_ = os.WriteFile(signers, []byte("alice "+strings.TrimSpace(string(pubLine))+"\n"), 0o644)
	sigFile := filepath.Join(dir, "msg.sig")
	_ = os.WriteFile(sigFile, sig, 0o644)
	cmd := exec.Command(sshKeygen(t), "-Y", "verify", "-f", signers, "-I", "alice", "-n", NSPlacement, "-s", sigFile)
	cmd.Stdin = bytes.NewReader(msg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen doesn't verify our signature: %v: %s", err, out)
	}

	cmd = exec.Command(sshKeygen(t), "-q", "-Y", "sign", "-f", path, "-n", NSSeal)
	cmd.Stdin = bytes.NewReader(msg)
	theirs, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifySignature(theirs, NSSeal, msg)
	if err != nil || !got.Equal(pub) {
		t.Fatalf("we don't verify ssh-keygen's signature: %v", err)
	}

	if _, err := VerifySignature(sig, NSSeal, msg); err == nil {
		t.Fatal("a signature for one namespace passed for another")
	}
	if _, err := VerifySignature(sig, NSPlacement, append(msg, 'x')); err == nil {
		t.Fatal("a signature passed over changed bytes")
	}
	if fp := pub.Fingerprint(); !strings.HasPrefix(fp, "SHA256:") {
		t.Fatal(fp)
	}
	out, _ := exec.Command(sshKeygen(t), "-l", "-f", path+".pub").Output()
	if !strings.Contains(string(out), pub.Fingerprint()) {
		t.Fatalf("fingerprint %s isn't ssh-keygen's: %s", pub.Fingerprint(), out)
	}
}

// A key with a passphrase is signed with by ssh-keygen; a key others can
// read is refused, as ssh refuses it.
func TestPrivateKeyFiles(t *testing.T) {
	dir := t.TempDir()
	path := newKey(t, dir, "bob")
	_ = os.Chmod(path, 0o644)
	if _, err := LoadPrivateKey(path); err == nil || !strings.Contains(err.Error(), "owner alone") {
		t.Fatalf("a readable private key was accepted: %v", err)
	}
	_ = os.Chmod(path, 0o600)
	enc := filepath.Join(dir, "enc")
	if out, err := exec.Command(sshKeygen(t), "-q", "-t", "ed25519", "-N", "secret", "-f", enc).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	k, err := LoadPrivateKey(enc)
	if err != nil {
		t.Fatal(err)
	}
	if k.external == "" {
		t.Fatal("an encrypted key was read natively")
	}
	if _, err := ParsePublicKey("ssh-rsa AAAAB3NzaC1yc2E alice"); err == nil {
		t.Fatal("an rsa key was taken")
	}
}

// A letter is the block and at most one signature of each kind, nothing
// else; Decode reads a whole letter's block.
func TestLetters(t *testing.T) {
	j := &Job{ID: "j1", Kind: "review", AskedBy: "alice", From: "region-a", Expires: "2099-01-01T00:00:00Z"}
	block := j.Encode()
	sig := []byte(sigBegin + "\nAAAA\n" + sigEnd + "\n")
	l := SealLetter(block, sig, sig)
	b, sigs, err := SplitLetter(l)
	if err != nil || !bytes.Equal(b, block) || len(sigs) != 2 {
		t.Fatalf("round trip: %v %d", err, len(sigs))
	}
	if got, err := Decode(l); err != nil || got.From != "region-a" {
		t.Fatalf("decode a letter: %v", err)
	}
	for name, bad := range map[string][]byte{
		"junk after":      append(append([]byte{}, block...), []byte("hello\n")...),
		"two requesters":  append(SealLetter(block, sig, nil), []byte("```signature requester\nx\n```\n")...),
		"an unknown kind": append(append([]byte{}, block...), []byte("```signature friend\nx\n```\n")...),
		"unterminated":    append(append([]byte{}, block...), []byte("```signature region\nx\n")...),
	} {
		if _, _, err := SplitLetter(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	bad := &Job{ID: "j2", Kind: "review", AskedBy: "bob@region-b", Expires: "2099-01-01T00:00:00Z"}
	if bad.Validate() == nil {
		t.Error("a qualified asked_by passed")
	}
}
