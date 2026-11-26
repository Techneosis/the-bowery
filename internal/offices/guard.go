package offices

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// statOwner is the real stat behind chainProblem: owner and mode, not
// following a final symlink (a symlink anywhere is judged by what it is,
// and its target is reached through its own checked parents).
func statOwner(p string) (uint32, os.FileMode, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%s: cannot read its owner", p)
	}
	return st.Uid, fi.Mode(), nil
}

type ownerError struct {
	path string
	uid  uint32
}

func (e *ownerError) Error() string { return fmt.Sprintf("%s: owned by uid %d", e.path, e.uid) }

// chainProblem checks path and every parent directory up to and including
// top: each must be owned by an allowed uid, and none writable by group or
// others (a sticky bit doesn't help: others could still create beside it).
// A file is only as safe as the directories that could swap it.
func chainProblem(path, top string, stat func(string) (uint32, os.FileMode, error), allow func(uint32) bool) error {
	p := filepath.Clean(path)
	top = filepath.Clean(top)
	for {
		uid, mode, err := stat(p)
		if err != nil {
			return err
		}
		if !allow(uid) {
			return &ownerError{p, uid}
		}
		if mode.Perm()&0o022 != 0 {
			return fmt.Errorf("%s: writable by group or others", p)
		}
		if p == top || p == "/" || !strings.HasPrefix(p, top) {
			return nil
		}
		p = filepath.Dir(p)
	}
}

// openPinned opens a plugin or fact provider, checks it in root mode
// (root-owned, with every parent), and hashes what it opened. The caller
// runs the returned file itself (runPlugin), never the path again, so what
// runs is what was hashed. An error here is an integrity failure.
func openPinned(path, pin string) (*os.File, string, error) {
	if err := checkRootOwned(path); err != nil {
		return nil, "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("can't read the plugin: %v", err)
	}
	if !devMode.Load() {
		fi, err := f.Stat()
		st, ok := fi.Sys().(*syscall.Stat_t)
		if err != nil || !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
			f.Close()
			return nil, "", fmt.Errorf("%s: not owned by root", path)
		}
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return nil, "", fmt.Errorf("can't read the plugin: %v", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if pin == "" {
		f.Close()
		return nil, got, fmt.Errorf("it has no pinned sha256")
	}
	if !strings.EqualFold(got, pin) {
		f.Close()
		return nil, got, fmt.Errorf("it doesn't match its pinned sha256")
	}
	return f, got, nil
}
