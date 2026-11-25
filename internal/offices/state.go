package offices

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is the owner's override (spec §3.3): open or closed, optionally
// until a time, and only for some kinds or one scope.
type State struct {
	State string   `json:"state"`
	Until string   `json:"until,omitempty"`
	Kinds []string `json:"kinds,omitempty"`
	Scope string   `json:"scope,omitempty"`
	By    string   `json:"by"`
	At    string   `json:"at"`
}

// ReadState returns nil when there is no override.
func ReadState(l Layout, office string) (*State, error) {
	b, err := os.ReadFile(l.StateFile(office))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := &State{}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("%s: %v", l.StateFile(office), err)
	}
	if s.State != "open" && s.State != "closed" {
		return nil, fmt.Errorf("%s: state must be open or closed", l.StateFile(office))
	}
	return s, nil
}

// WriteState writes the override (nil clears it), atomically.
func WriteState(l Layout, office string, s *State) error {
	p := l.StateFile(office)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if s == nil {
		err := os.Remove(p)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	b, _ := json.Marshal(s)
	return WriteAtomic(p, append(b, '\n'), 0o644)
}

// Applies says whether the override speaks to this job now.
func (s *State) Applies(job *Job, now time.Time) bool {
	if s == nil {
		return false
	}
	if s.Until != "" {
		if t, err := time.Parse(time.RFC3339, s.Until); err == nil && !now.Before(t) {
			return false
		}
	}
	if len(s.Kinds) > 0 && (job == nil || !contains(s.Kinds, job.Kind)) {
		return false
	}
	if s.Scope != "" && (job == nil || job.Scope != s.Scope) {
		return false
	}
	return true
}

func WriteAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
