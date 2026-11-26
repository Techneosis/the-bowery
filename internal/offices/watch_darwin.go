//go:build darwin

package offices

import (
	"context"
	"sync"
	"syscall"
	"time"
)

// dirWatch is a kqueue watch on one directory (EVFILT_VNODE, NOTE_WRITE):
// it fires when an entry is created, renamed in or out, or removed, so the
// inbox is a true long-poll on macOS too, as inotify makes it on Linux. A
// pipe in the same queue lets ctx end the wait.
type dirWatch struct {
	mu     sync.Mutex
	closed bool
	kq, fd int
	pr, pw int
}

func newDirWatch(dir string) (*dirWatch, error) {
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	kq, err := syscall.Kqueue()
	if err != nil {
		syscall.Close(fd)
		return nil, err
	}
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		syscall.Close(fd)
		syscall.Close(kq)
		return nil, err
	}
	syscall.CloseOnExec(p[0])
	syscall.CloseOnExec(p[1])
	w := &dirWatch{kq: kq, fd: fd, pr: p[0], pw: p[1]}
	ch := make([]syscall.Kevent_t, 2)
	syscall.SetKevent(&ch[0], fd, syscall.EVFILT_VNODE, syscall.EV_ADD|syscall.EV_CLEAR)
	ch[0].Fflags = syscall.NOTE_WRITE | syscall.NOTE_DELETE | syscall.NOTE_RENAME | syscall.NOTE_EXTEND
	syscall.SetKevent(&ch[1], p[0], syscall.EVFILT_READ, syscall.EV_ADD)
	if _, err := syscall.Kevent(kq, ch, nil, nil); err != nil {
		w.close()
		return nil, err
	}
	return w, nil
}

// newAppendWatch is the directory's watch: kqueue on a directory doesn't see
// an append to a file in it, so a bus follower there is woken by the node
// itself, or by its backstop.
func newAppendWatch(dir string) (*dirWatch, error) { return newDirWatch(dir) }

// wait blocks until an event, the deadline, or ctx; true means an event.
func (w *dirWatch) wait(ctx context.Context, deadline time.Time) (bool, error) {
	stop := context.AfterFunc(ctx, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if !w.closed {
			_, _ = syscall.Write(w.pw, []byte{0})
		}
	})
	defer stop()
	ev := make([]syscall.Kevent_t, 2)
	for {
		d := time.Until(deadline)
		if d <= 0 {
			return false, nil
		}
		ts := syscall.NsecToTimespec(int64(d))
		n, err := syscall.Kevent(w.kq, nil, ev, &ts)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		if n == 0 {
			return false, nil
		}
		for _, e := range ev[:n] {
			if int(e.Ident) == w.fd {
				return true, nil
			}
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
	}
}

func (w *dirWatch) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	for _, fd := range []int{w.fd, w.kq, w.pr, w.pw} {
		syscall.Close(fd)
	}
}
