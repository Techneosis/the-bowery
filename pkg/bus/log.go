package bus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
)

var (
	// ErrFork is a record at a seq the log has, with a different event:
	// the producer has said two things under one number.
	ErrFork = errors.New("a different event at a seq already in the log")
	// ErrBehind is a record older than the producer's head that the log
	// doesn't have: it was skipped, so it can't be filled in now.
	ErrBehind = errors.New("older than the producer's head")
)

// Log is a node's store: one append-only file per producer, one record per
// line, in seq order. Every record is verified before it's appended; what
// is on disk is trusted, so opening a log checks its shape and order, not
// its signatures again.
//
// Several handles (in one process or several) may share a directory: each
// producer's file is locked while it's read or appended to, and a handle
// catches up with what the others wrote before it decides anything.
type Log struct {
	dir   string
	keys  Keys
	mu    sync.Mutex
	prods map[string]*plog
}

type plog struct {
	producer string
	f        *os.File
	seqs     []uint64
	offs     []int64 // each record's line starts here
	size     int64   // the bytes indexed so far, whole lines only
}

func fileName(producer string) string { return strings.ReplaceAll(producer, "/", "+") + ".log" }

func producerOf(name string) (string, bool) {
	base, ok := strings.CutSuffix(name, ".log")
	p := strings.ReplaceAll(base, "+", "/")
	return p, ok && ValidProducer(p)
}

// OpenLog opens (or makes) the log in dir. A torn last line, left by a
// crash mid-append, is cut off; any other line that doesn't read, or a seq
// out of order, is corruption and the log refuses to open.
func OpenLog(dir string, keys Keys) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Log{dir: dir, keys: keys, prods: map[string]*plog{}}
	if err := l.refresh(true); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Close closes the log's files.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range l.prods {
		p.f.Close()
	}
	l.prods = map[string]*plog{}
	return nil
}

// refresh opens any producer file another handle has made, and catches up
// on every one. trim cuts a torn tail (under an exclusive lock).
func (l *Log) refresh(trim bool) error {
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	for _, ent := range ents {
		p, ok := producerOf(ent.Name())
		if !ok || !ent.Type().IsRegular() {
			continue
		}
		if _, err := l.open(p); err != nil {
			return err
		}
	}
	for _, p := range l.prods {
		if err := l.locked(p, trim, func() error { return p.catchUp(trim) }); err != nil {
			return err
		}
	}
	return nil
}

func (l *Log) open(producer string) (*plog, error) {
	if p := l.prods[producer]; p != nil {
		return p, nil
	}
	path := filepath.Join(l.dir, fileName(producer))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	p := &plog{producer: producer, f: f}
	l.prods[producer] = p
	return p, nil
}

func (l *Log) locked(p *plog, exclusive bool, f func() error) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(p.f.Fd()), how); err != nil {
		return err
	}
	defer syscall.Flock(int(p.f.Fd()), syscall.LOCK_UN)
	return f()
}

// catchUp indexes the whole lines past what's indexed.
func (p *plog) catchUp(trim bool) error {
	fi, err := p.f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() < p.size {
		return fmt.Errorf("%s: the log shrank (%d < %d bytes)", p.f.Name(), fi.Size(), p.size)
	}
	if fi.Size() == p.size {
		return nil
	}
	buf := make([]byte, fi.Size()-p.size)
	if _, err := p.f.ReadAt(buf, p.size); err != nil && err != io.EOF {
		return err
	}
	off := p.size
	for len(buf) > 0 {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break // a torn tail, or another handle mid-write
		}
		line := buf[:i+1]
		r, err := ParseLine(line)
		if err != nil {
			return fmt.Errorf("%s at byte %d: %v", p.f.Name(), off, err)
		}
		if r.Event.Producer != p.producer {
			return fmt.Errorf("%s at byte %d: a record of %s", p.f.Name(), off, r.Event.Producer)
		}
		if n := len(p.seqs); n > 0 && r.Event.Seq <= p.seqs[n-1] {
			return fmt.Errorf("%s at byte %d: seq %d after %d", p.f.Name(), off, r.Event.Seq, p.seqs[n-1])
		}
		p.seqs = append(p.seqs, r.Event.Seq)
		p.offs = append(p.offs, off)
		off += int64(len(line))
		buf = buf[i+1:]
	}
	p.size = off
	if len(buf) > 0 && trim {
		return p.f.Truncate(p.size)
	}
	return nil
}

func (p *plog) head() uint64 {
	if len(p.seqs) == 0 {
		return 0
	}
	return p.seqs[len(p.seqs)-1]
}

func (p *plog) at(i int) (Record, error) {
	end := p.size
	if i+1 < len(p.offs) {
		end = p.offs[i+1]
	}
	buf := make([]byte, end-p.offs[i])
	if _, err := p.f.ReadAt(buf, p.offs[i]); err != nil && err != io.EOF {
		return Record{}, err
	}
	return ParseLine(buf)
}

// Append verifies r and adds it. It reports false, with no error, for an
// exact duplicate of a record the log has (a resend), ErrFork for a
// different event at a seq it has, and ErrBehind for one older than the
// producer's head that it doesn't have.
func (l *Log) Append(r Record) (bool, error) {
	if err := r.Verify(l.keys); err != nil {
		return false, err
	}
	line, err := r.Line()
	if err != nil {
		return false, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	p, err := l.open(r.Event.Producer)
	if err != nil {
		return false, err
	}
	added := false
	err = l.locked(p, true, func() error {
		if err := p.catchUp(true); err != nil {
			return err
		}
		seq := r.Event.Seq
		if seq > p.head() {
			if _, err := p.f.Write(line); err != nil {
				return err
			}
			if err := p.f.Sync(); err != nil {
				return err
			}
			p.seqs = append(p.seqs, seq)
			p.offs = append(p.offs, p.size)
			p.size += int64(len(line))
			added = true
			return nil
		}
		i := sort.Search(len(p.seqs), func(i int) bool { return p.seqs[i] >= seq })
		if i == len(p.seqs) || p.seqs[i] != seq {
			return fmt.Errorf("%v: %w (%d)", r.Ref(), ErrBehind, p.head())
		}
		have, err := p.at(i)
		if err != nil {
			return err
		}
		a, _ := have.Event.Canonical()
		b, _ := r.Event.Canonical()
		if !bytes.Equal(a, b) {
			return fmt.Errorf("%v: %w", r.Ref(), ErrFork)
		}
		return nil
	})
	return added, err
}

// Heads is the last seq the log has from each producer.
func (l *Log) Heads() Cursor {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.refresh(false)
	c := Cursor{}
	for name, p := range l.prods {
		if h := p.head(); h > 0 {
			c[name] = h
		}
	}
	return c
}

// Since reads the records past c, producer by producer (in name order),
// each in seq order; at most limit of them (0 is no limit).
func (l *Log) Since(c Cursor, limit int) ([]Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.refresh(false); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(l.prods))
	for n := range l.prods {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Record
	for _, n := range names {
		p := l.prods[n]
		after := c[n]
		err := l.locked(p, false, func() error {
			i := sort.Search(len(p.seqs), func(i int) bool { return p.seqs[i] > after })
			for ; i < len(p.seqs); i++ {
				if limit > 0 && len(out) >= limit {
					return nil
				}
				r, err := p.at(i)
				if err != nil {
					return err
				}
				out = append(out, r)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
