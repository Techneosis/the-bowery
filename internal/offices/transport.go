package offices

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Letter is one placement waiting in an office's inbox.
type Letter struct {
	ID   string // the file's name without .placement: the transport's message id
	Path string
	Body []byte
	Seen time.Time // when intake first listed it
	// Sent is when the transport says the letter was sent (zero if it
	// can't say): a time for the latency record, never for identity.
	Sent time.Time
}

// Received is one catalog a peer region published to this one.
type Received struct {
	Machine string
	Body    []byte
	At      time.Time // when it arrived
}

// Transport is the placement transport (spec §10): the one thing that
// carries placements to an office's inbox and replies back to an asker. It
// is never trusted for identity: the office checks every placement itself.
// Nothing outside a Transport's own implementation knows how letters move,
// so another mailbox can be plugged in by name without touching the
// resolver or the pull job.
//
// An address says where a region's boxes and catalogs are reached (a peer
// entry's `address`); "" is this region's own. What an address looks like
// is the transport's business alone.
type Transport interface {
	// Send delivers one placement to a box at an address.
	Send(addr, box, id string, body []byte) error
	// Reply delivers a line to an asker's box at an address (a refusal, an
	// expiry).
	Reply(addr, asker, id string, body []byte) error
	// Inbox opens a box of this region's for its office's pull job.
	Inbox(box string) Inbox
	// Publish delivers region from's catalog of one machine's offices to
	// peer (reached at addr), replacing the one it sent before. A transport
	// may push it (dir) or keep it for the peer to fetch (http).
	Publish(peer, addr, from, machine string, body []byte) error
	// Catalogs are the catalogs region from (reached at addr) has published
	// to region self, one per machine.
	Catalogs(from, addr, self string) ([]Received, error)
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
// `transport`). "" and "dir" are the directory transport, "http" the HTTP
// one; another name is an adapter this build doesn't have.
func NewTransport(kind string, l Layout, poll time.Duration) (Transport, error) {
	switch kind {
	case "", "dir":
		return &DirTransport{L: l, Poll: poll}, nil
	case "http":
		return NewHTTPTransport(l, poll), nil
	}
	return nil, fmt.Errorf("transport %q isn't built into this binary (this one has: dir, http)", kind)
}

// DirTransport is the built-in transport: a box is a directory under the
// layout's var/boxes, a placement is a file `<id>.placement` dropped into
// it, and a reply is `<id>.reply` in the asker's box. Catalogs from a peer
// are var/catalogs/<region>/<machine>.catalog. An address is another
// region's var directory (an absolute path), so two regions on one machine
// (dev mode, or two roots) reach each other. It is what dev mode and a
// single machine use.
type DirTransport struct {
	L    Layout
	Poll time.Duration
}

// at is the var directory an address names.
func (t *DirTransport) at(addr string) (string, error) {
	if addr == "" {
		return t.L.Var, nil
	}
	if !filepath.IsAbs(addr) || filepath.Clean(addr) != addr {
		return "", fmt.Errorf("the dir transport's address is a clean absolute path to a region's var directory (got %q)", addr)
	}
	return addr, nil
}

func boxName(box string) error {
	// A box is a name, never a path out of the boxes.
	if box == "" || strings.ContainsAny(box, "/\\") || strings.Contains(box, "..") {
		return fmt.Errorf("%q isn't a box name", box)
	}
	return nil
}

func (t *DirTransport) Send(addr, box, id string, body []byte) error {
	v, err := t.at(addr)
	if err != nil {
		return err
	}
	if err := boxName(box); err != nil {
		return err
	}
	return dropFile(filepath.Join(v, "boxes", box), id+".placement", body)
}

func (t *DirTransport) Reply(addr, asker, id string, body []byte) error {
	v, err := t.at(addr)
	if err != nil {
		return err
	}
	if err := boxName(asker); err != nil {
		return fmt.Errorf("asker %q isn't a box name", asker)
	}
	return dropFile(filepath.Join(v, "boxes", "asker-"+asker), id+".reply", body)
}

func (t *DirTransport) Publish(_, addr, from, machine string, body []byte) error {
	v, err := t.at(addr)
	if err != nil {
		return err
	}
	if !NameRE.MatchString(from) || !LabelRE.MatchString(machine) || strings.ContainsAny(machine, "/") {
		return fmt.Errorf("region %q or machine %q isn't a name", from, machine)
	}
	dir := filepath.Join(v, "catalogs", from)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return WriteAtomic(filepath.Join(dir, machine+".catalog"), body, 0o644)
}

func (t *DirTransport) Catalogs(from, _, _ string) ([]Received, error) {
	if !NameRE.MatchString(from) {
		return nil, fmt.Errorf("%q isn't a region name", from)
	}
	return readCatalogDir(filepath.Join(t.L.Var, "catalogs", from))
}

// readCatalogDir reads every <machine>.catalog in dir, by machine.
func readCatalogDir(dir string) ([]Received, error) {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Received
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".catalog") || strings.HasPrefix(n, ".") {
			continue
		}
		p := filepath.Join(dir, n)
		b, err := readRegular(p)
		if err != nil {
			continue
		}
		r := Received{Machine: strings.TrimSuffix(n, ".catalog"), Body: b}
		if fi, err := os.Stat(p); err == nil {
			r.At = fi.ModTime()
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Machine < out[k].Machine })
	return out, nil
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
		return ErrInBox
	}
	return writeNew(dst, body)
}

// writeNew puts body at dst only if nothing is there: the file is written
// whole under a temporary name and then linked into place, which fails if dst
// exists, so two writers of one id can't replace each other (a stat before a
// rename can't promise that). Where hard links aren't allowed it falls back to
// a rename.
func writeNew(dst string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	switch err := os.Link(tmp.Name(), dst); {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrExist):
		return ErrInBox
	default:
		return os.Rename(tmp.Name(), dst)
	}
}

// ErrInBox is a letter whose id is already waiting in the box.
var ErrInBox = errors.New("a letter with that id is already in the box")

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
		b, err := readRegular(p)
		if err != nil {
			continue
		}
		l := Letter{ID: strings.TrimSuffix(n, ".placement"), Path: p, Body: b, Seen: time.Now()}
		if fi, err := e.Info(); err == nil {
			l.Sent = fi.ModTime() // written once, by a rename: the send
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out, nil
}

// Done files what was read, not what is at the path now: a letter swapped in
// after it was verified must not become the accepted (or refused) record.
func (d *DirInbox) Done(l Letter, to string) error {
	if to == "" {
		return os.Remove(l.Path)
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	if err := WriteAtomic(filepath.Join(to, filepath.Base(l.Path)), l.Body, 0o644); err != nil {
		return err
	}
	return os.Remove(l.Path)
}

// maxLetter bounds what a box or a catalog directory is read for: a letter or
// a catalog is a few KiB, and the reader holds a lock.
const maxLetter = 4 << 20

// readRegular reads a regular file a peer or asker may have written, and
// nothing else: a FIFO would block the reader for ever, a symlink would read
// wherever it points, and a huge file would fill memory.
func readRegular(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > maxLetter {
		return nil, fmt.Errorf("%s isn't a regular file of a sane size", path)
	}
	return io.ReadAll(io.LimitReader(f, maxLetter+1))
}
