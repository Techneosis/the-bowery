//go:build linux

package offices

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// dirWatch is an inotify watch on one directory: Wait returns as soon as an
// entry is created, moved in or out, written or removed, so the directory
// transport's inbox is a true long-poll on Linux.
type dirWatch struct{ f *os.File }

func newDirWatch(dir string) (*dirWatch, error) { return newWatch(dir, 0) }

// newAppendWatch also wakes on a write to a file that stays open (an
// append-only log's).
func newAppendWatch(dir string) (*dirWatch, error) { return newWatch(dir, syscall.IN_MODIFY) }

func newWatch(dir string, extra uint32) (*dirWatch, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	mask := extra | uint32(syscall.IN_CREATE|syscall.IN_MOVED_TO|syscall.IN_MOVED_FROM|syscall.IN_CLOSE_WRITE|
		syscall.IN_DELETE|syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF)
	if _, err := syscall.InotifyAddWatch(fd, dir, mask); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	// A non-blocking fd handed to os.NewFile is driven by the runtime's
	// poller, so a read deadline can end the wait.
	return &dirWatch{f: os.NewFile(uintptr(fd), "inotify:"+dir)}, nil
}

// wait blocks until an event, the deadline, or ctx; true means an event.
func (w *dirWatch) wait(ctx context.Context, deadline time.Time) (bool, error) {
	if err := w.f.SetReadDeadline(deadline); err != nil {
		return false, err
	}
	stop := context.AfterFunc(ctx, func() { _ = w.f.SetReadDeadline(time.Now()) })
	defer stop()
	buf := make([]byte, 4096)
	_, err := w.f.Read(buf)
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return false, nil
	}
	return false, err
}

func (w *dirWatch) close() { w.f.Close() }
