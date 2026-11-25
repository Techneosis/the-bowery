package offices

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Letter is one placement waiting in an office's inbox.
type Letter struct {
	ID   string // the file's name without .placement: the transport's message id
	Path string
	Body []byte
	Seen time.Time // when intake first listed it
}

// Transport is the placement transport (spec §10): the one thing that
// carries placements to an office's inbox and replies back to an asker. It
// is never trusted for identity: the office checks every placement itself.
// Nothing outside a Transport's own implementation knows how letters move,
// so another mailbox can be plugged in by name without touching the
// resolver or the pull job.
type Transport interface {
	// Send delivers one placement to a box.
	Send(box, id string, body []byte) error
	// Reply delivers a line to an asker's box (a refusal, an expiry).
	Reply(asker, id string, body []byte) error
	// Inbox opens a box for its office's pull job.
	Inbox(box string) Inbox
}

// Inbox is one office's side of the transport. The pull loop calls Wait,
// then List, then hands each letter to Pull. Wait returns as soon as there
// may be something new, or when the backstop passes, whichever is first, so
// a transport that can long-poll (a mailbox's "wait") implements Wait by
// blocking on it, and the loop needs no change. The directory transport
// blocks on a watch of the box where the platform has one (Linux), and
// polls on an interval elsewhere.
type Inbox interface {
	Wait(ctx context.Context, backstop time.Duration) (bool, error) // true: the box changed
	List() ([]Letter, error)
	Done(l Letter, to string) error // move a letter out of the inbox into folder `to` ("" removes it)
}

// NewTransport picks a transport by name (the machine file's and bowery's
// `transport`). "" and "dir" are the directory transport; another name is
// an adapter this build doesn't have.
func NewTransport(kind string, l Layout, poll time.Duration) (Transport, error) {
	switch kind {
	case "", "dir":
		return &DirTransport{L: l, Poll: poll}, nil
	}
	return nil, fmt.Errorf("transport %q isn't built into this binary (this one has: dir)", kind)
}

// DirTransport is the built-in transport: a box is a directory under the
// layout's var/boxes, a placement is a file `<id>.placement` dropped into
// it, and a reply is `<id>.reply` in the asker's box. It is what dev mode
// and a single machine use.
type DirTransport struct {
	L    Layout
	Poll time.Duration
}

func (t *DirTransport) Send(box, id string, body []byte) error {
	return dropFile(t.L.BoxDir(box), id+".placement", body)
}

func (t *DirTransport) Reply(asker, id string, body []byte) error {
	// An asker names a box, never a path out of the boxes.
	if asker == "" || strings.ContainsAny(asker, "/\\") || strings.Contains(asker, "..") {
		return fmt.Errorf("asker %q isn't a box name", asker)
	}
	return dropFile(t.L.BoxDir("asker-"+asker), id+".reply", body)
}

func (t *DirTransport) Inbox(box string) Inbox {
	return &DirInbox{Dir: t.L.BoxDir(box), Poll: t.Poll}
}

// dropFile writes a letter so a reader never sees half of it: a hidden
// temp file in the box, then a rename.
func dropFile(dir, name string, body []byte) error {
	if !IDRE.MatchString(strings.TrimSuffix(strings.TrimSuffix(name, ".placement"), ".reply")) {
		return fmt.Errorf("letter name %q", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, name)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s is already in the box", name)
	}
	return WriteAtomic(dst, body, 0o644)
}

// DirInbox is a directory box, polled.
type DirInbox struct {
	Dir  string
	Poll time.Duration // how often Wait looks; default 1s
	last string
}

func (d *DirInbox) snapshot() string {
	ents, _ := os.ReadDir(d.Dir)
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".placement") {
			names = append(names, e.Name())
		}
	}
	return strings.Join(names, "\n")
}

// Wait is a long-poll: it returns as soon as the box's placements change,
// or false at the backstop. On Linux it blocks on an inotify watch of the
// box, so a letter is seen the moment it lands; elsewhere it looks every
// Poll.
func (d *DirInbox) Wait(ctx context.Context, backstop time.Duration) (bool, error) {
	poll := d.Poll
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(backstop)
	_ = os.MkdirAll(d.Dir, 0o755)
	for {
		if s := d.snapshot(); s != d.last {
			d.last = s
			return true, nil
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		if w, err := newDirWatch(d.Dir); err == nil {
			// A letter that landed before the watch was in place.
			if s := d.snapshot(); s != d.last {
				w.close()
				d.last = s
				return true, nil
			}
			_, werr := w.wait(ctx, deadline)
			w.close()
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if werr == nil {
				continue // look again: an event, or the deadline
			}
			// The watch broke: fall back to polling for this wait.
		}
		wait := poll
		if r := time.Until(deadline); r < wait {
			wait = r
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (d *DirInbox) List() ([]Letter, error) {
	ents, err := os.ReadDir(d.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Letter
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".placement") || strings.HasPrefix(n, ".") {
			continue
		}
		p := filepath.Join(d.Dir, n)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		out = append(out, Letter{ID: strings.TrimSuffix(n, ".placement"), Path: p, Body: b, Seen: time.Now()})
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out, nil
}

func (d *DirInbox) Done(l Letter, to string) error {
	if to == "" {
		return os.Remove(l.Path)
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(to, filepath.Base(l.Path))
	if err := os.Rename(l.Path, dst); err != nil {
		// Another filesystem: copy, then remove.
		if werr := os.WriteFile(dst, l.Body, 0o644); werr != nil {
			return werr
		}
		return os.Remove(l.Path)
	}
	return nil
}
