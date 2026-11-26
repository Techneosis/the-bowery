package offices

import (
	"context"
	"path/filepath"
	"time"
)

// The bus node's files, under the region's var directory: its log (one file
// per producer) and the unix socket its local seats connect to.
func (l Layout) BusDir() string    { return filepath.Join(l.Var, "bus") }
func (l Layout) BusLog() string    { return filepath.Join(l.Var, "bus", "log") }
func (l Layout) BusSocket() string { return filepath.Join(l.Var, "bus", "node.sock") }

// Watch wakes on a change in one directory, appends to its files included
// where the platform reports them (inotify does; kqueue on a directory
// doesn't, and the caller's backstop covers it).
type Watch struct{ w *dirWatch }

// WatchAppends watches dir's entries and the files in it.
func WatchAppends(dir string) (*Watch, error) {
	w, err := newAppendWatch(dir)
	if err != nil {
		return nil, err
	}
	return &Watch{w: w}, nil
}

// Wait blocks until a change, the deadline, or ctx; true means a change.
func (w *Watch) Wait(ctx context.Context, deadline time.Time) (bool, error) {
	return w.w.wait(ctx, deadline)
}

// Close ends the watch.
func (w *Watch) Close() { w.w.close() }
