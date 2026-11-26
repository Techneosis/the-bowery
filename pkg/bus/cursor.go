package bus

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Cursor is a reader's position: the last seq it has from each producer.
// A producer it doesn't name is read from its start.
type Cursor map[string]uint64

// ParseCursor reads `producer=seq,producer=seq`, the form String writes and
// a Last-Event-ID carries. The empty string is the empty cursor.
func ParseCursor(s string) (Cursor, error) {
	c := Cursor{}
	if s == "" {
		return c, nil
	}
	for _, part := range strings.Split(s, ",") {
		p, n, ok := strings.Cut(part, "=")
		if !ok || !ValidProducer(p) {
			return nil, fmt.Errorf("cursor part %q isn't producer=seq", part)
		}
		seq, err := strconv.ParseUint(n, 10, 64)
		if err != nil || seq == 0 {
			return nil, fmt.Errorf("cursor part %q: seq isn't a count from 1", part)
		}
		if _, dup := c[p]; dup {
			return nil, fmt.Errorf("cursor names %s twice", p)
		}
		c[p] = seq
	}
	return c, nil
}

// String writes the cursor sorted by producer, so one position has one
// spelling.
func (c Cursor) String() string {
	ps := make([]string, 0, len(c))
	for p, seq := range c {
		if seq > 0 {
			ps = append(ps, p)
		}
	}
	sort.Strings(ps)
	for i, p := range ps {
		ps[i] = p + "=" + strconv.FormatUint(c[p], 10)
	}
	return strings.Join(ps, ",")
}

// Advance moves the cursor past r; it never moves back.
func (c Cursor) Advance(r Ref) {
	if r.Seq > c[r.Producer] {
		c[r.Producer] = r.Seq
	}
}

// Covers says whether a reader at c has already seen r.
func (c Cursor) Covers(r Ref) bool { return r.Seq <= c[r.Producer] }

// Clone is a copy the caller may change.
func (c Cursor) Clone() Cursor {
	out := make(Cursor, len(c))
	for p, s := range c {
		out[p] = s
	}
	return out
}
