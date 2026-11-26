package bus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Techneosis/the-bowery/internal/offices"
)

func region(t *testing.T, bus string) *offices.Region {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o700); err != nil {
		t.Fatal(err)
	}
	text := "region = \"north\"\nkey = \"\"\n" + bus
	if err := os.WriteFile(filepath.Join(dir, "etc", "region.toml"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := offices.LoadRegion(offices.Layout{Dev: true, Etc: filepath.Join(dir, "etc"), Var: filepath.Join(dir, "var")})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const busConfig = `
[bus]
retention = "240h"
private   = ["private.*", "seat.note"]

[bus.retain]
"job.*"       = "24h"
"job.started" = "1h"

[bus.seats.alice]
publish = ["job.*", "private.*"]
read    = ["*"]

[bus.seats.bob]
read = ["card.answered"]

[peers.south]
address     = "/srv/south"
may_publish = ["job.*"]
may_read    = ["card.*", "private.*", "*"]

[peers.east]
address = "/srv/east"
`

// Topic rules default to deny: a peer or seat with no list gets nothing,
// and a list says exactly which topics go which way.
func TestRulesPeers(t *testing.T) {
	r := RulesOf(region(t, busConfig))
	cases := []struct {
		peer, kind string
		dir        Dir
		want       bool
	}{
		{"south", "job.started", Publish, true},
		{"south", "job.started.late", Publish, true},
		{"south", "jobs.started", Publish, false}, // a prefix is a whole part
		{"south", "card.answered", Publish, false},
		{"south", "card.answered", Read, true},
		{"south", "gate.decided", Read, true},   // "*"
		{"south", "private.card", Read, false},  // private never leaves, even under "*"
		{"south", "seat.note", Read, false},     // an exact private topic too
		{"south", "seat.notes", Read, true},     // ...and only that one
		{"east", "card.answered", Read, false},  // no lists: nothing either way
		{"east", "job.started", Publish, false}, //
		{"west", "card.answered", Read, false},  // not a peer at all
	}
	for _, c := range cases {
		if got := r.Allowed(c.peer, c.kind, c.dir); got != c.want {
			t.Errorf("Allowed(%s, %s, %v) = %v, want %v", c.peer, c.kind, c.dir, got, c.want)
		}
	}
}

// A peer may not publish into this region's private topics either: they're
// this region's own.
func TestRulesPeerCannotPublishPrivate(t *testing.T) {
	r := RulesOf(region(t, `
[bus]
private = ["private.*"]
[peers.south]
address = "/srv/south"
may_publish = ["*"]
`))
	if r.Allowed("south", "private.card", Publish) {
		t.Fatal("a peer published into a private topic")
	}
	if !r.Allowed("south", "job.done", Publish) {
		t.Fatal(`"*" doesn't let a peer publish job.done`)
	}
}

// Local seats are keyed by Unix user; private topics are theirs to use.
func TestRulesSeats(t *testing.T) {
	r := RulesOf(region(t, busConfig))
	cases := []struct {
		user, kind string
		dir        Dir
		want       bool
	}{
		{"alice", "private.card", Publish, true},
		{"alice", "private.card", Read, true},
		{"alice", "card.answered", Publish, false},
		{"bob", "card.answered", Read, true},
		{"bob", "card.answered", Publish, false},
		{"bob", "card.opened", Read, false},
		{"carol", "card.answered", Read, false},
	}
	for _, c := range cases {
		if got := r.SeatAllowed(c.user, c.kind, c.dir); got != c.want {
			t.Errorf("SeatAllowed(%s, %s, %v) = %v, want %v", c.user, c.kind, c.dir, got, c.want)
		}
	}
	if !r.Private("private.card") || r.Private("card.answered") {
		t.Fatal("Private reads the list wrong")
	}
}

// Retention is per topic: the most specific pattern wins, then the
// region's default, then 30 days.
func TestRulesRetention(t *testing.T) {
	r := RulesOf(region(t, busConfig))
	for kind, want := range map[string]time.Duration{
		"job.started":   time.Hour,
		"job.done":      24 * time.Hour,
		"card.answered": 240 * time.Hour,
	} {
		if got := r.Retention(kind); got != want {
			t.Errorf("Retention(%s) = %v, want %v", kind, got, want)
		}
	}
	if got := RulesOf(region(t, "")).Retention("card.answered"); got != DefaultRetention {
		t.Errorf("default retention = %v, want %v", got, DefaultRetention)
	}
	if DefaultRetention != 30*24*time.Hour {
		t.Errorf("DefaultRetention = %v", DefaultRetention)
	}
}

// Compact drops what's past its topic's retention but always keeps each
// producer's head, so a resend is still told apart from a fork. A cursor
// below what was dropped gets ErrTooOld (a resync), one at or past it reads
// on.
func TestCompactAndTooOld(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	l, err := OpenLog(dir, w.keys)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := uint64(1); i <= 5; i++ {
		e := ev(i)
		e.At = base.Add(time.Duration(i) * time.Hour)
		if i == 2 {
			e.Kind = "card.answered" // kept longer
		}
		if _, err := l.Append(w.sign(t, e)); err != nil {
			t.Fatal(err)
		}
	}
	keep := func(kind string) time.Duration {
		if kind == "card.answered" {
			return 100 * time.Hour
		}
		return 2 * time.Hour
	}
	now := base.Add(10 * time.Hour) // seqs 1,3,4,5 at 1h..5h are past 2h; 5 is the head
	dropped, err := l.Compact(now, keep)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 3 {
		t.Fatalf("dropped %d, want 3 (seqs 1, 3, 4)", dropped)
	}
	all, err := l.Since(Cursor{"north/desk": 4}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Event.Seq != 5 {
		t.Fatalf("past the floor: %v", all)
	}
	_, err = l.Since(Cursor{"north/desk": 1}, 0)
	var too *TooOldError
	if !errors.Is(err, ErrTooOld) || !errors.As(err, &too) || too.Floors["north/desk"] != 4 {
		t.Fatalf("a cursor below the floor: %v", err)
	}
	if _, err := l.Since(Cursor{}, 0); !errors.Is(err, ErrTooOld) {
		t.Fatalf("an empty cursor reads past dropped records: %v", err)
	}
	if f := l.Floors(); f["north/desk"] != 4 {
		t.Fatalf("Floors = %v", f)
	}
	got, err := l.Since(l.Floors(), 0)
	if err != nil || len(got) != 1 || got[0].Event.Seq != 5 {
		t.Fatalf("from the floors: %v %v", got, err)
	}
	// A reader resyncing reads everything still kept, the longer-kept
	// seq 2 below the floor included, and pages through it.
	got, err = l.SinceKept(Cursor{}, 1)
	if err != nil || len(got) != 1 || got[0].Event.Seq != 2 {
		t.Fatalf("kept, first page: %v %v", got, err)
	}
	got, err = l.SinceKept(Cursor{"north/desk": 2}, 0)
	if err != nil || len(got) != 1 || got[0].Event.Seq != 5 {
		t.Fatalf("kept, second page: %v %v", got, err)
	}

	// The head is kept: a resend is still a duplicate, and seq 6 appends.
	h := ev(5)
	h.At = base.Add(5 * time.Hour)
	if added, err := l.Append(w.sign(t, h)); err != nil || added {
		t.Fatalf("a resend of the head after compaction: %v %v", added, err)
	}
	if added, err := l.Append(w.sign(t, ev(6))); err != nil || !added {
		t.Fatalf("seq 6 after compaction: %v %v", added, err)
	}

	// Another handle on the same directory, open since before, sees the
	// compacted file (seq 6, the head then, kept; floor 5) and the floor,
	// and appends to the new file.
	l2, err := OpenLog(dir, w.keys)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if _, err := l.Compact(now.Add(1000*time.Hour), keep); err != nil {
		t.Fatal(err)
	}
	if added, err := l2.Append(w.sign(t, ev(7))); err != nil || !added {
		t.Fatalf("seq 7 from the other handle: %v %v", added, err)
	}
	got, err = l.Since(l.Floors(), 0)
	if err != nil || len(got) != 2 || got[0].Event.Seq != 6 || got[1].Event.Seq != 7 {
		t.Fatalf("after the second compaction: %v %v", got, err)
	}
	if f := l2.Floors(); f["north/desk"] != 5 {
		t.Fatalf("the other handle's floors = %v", f)
	}

	// And it all survives a reopen.
	l.Close()
	l3, err := OpenLog(dir, w.keys)
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Close()
	if h := l3.Heads()["north/desk"]; h != 7 {
		t.Fatalf("head after reopen = %d", h)
	}
	if f := l3.Floors(); f["north/desk"] != 5 {
		t.Fatalf("floors after reopen = %v", f)
	}
}

// One handle appends while another compacts over and over: no append is
// lost to the swap (each lands in the file that's at the path), and the
// end state is the head alone with the floor just under it.
func TestCompactWhileAppending(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	a, err := OpenLog(dir, w.keys)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenLog(dir, w.keys)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	const n = 120
	recs := make([]Record, n+1)
	for i := uint64(1); i <= n; i++ {
		recs[i] = w.sign(t, ev(i))
	}
	never := func(string) time.Duration { return 0 }
	late := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC) // past every event
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= n; i++ {
			if added, err := a.Append(recs[i]); err != nil || !added {
				done <- fmt.Errorf("append %d: %v %v", i, added, err)
				return
			}
		}
		done <- nil
	}()
	for running := true; running; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			running = false
		default:
			if _, err := b.Compact(late, never); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := b.Compact(late, never); err != nil {
		t.Fatal(err)
	}
	if h := a.Heads()["north/desk"]; h != n {
		t.Fatalf("head = %d, want %d", h, n)
	}
	if f := b.Floors()["north/desk"]; f != n-1 {
		t.Fatalf("floor = %d, want %d", f, n-1)
	}
	got, err := a.SinceKept(Cursor{}, 0)
	if err != nil || len(got) != 1 || got[0].Event.Seq != n {
		t.Fatalf("kept after it all: %v %v", len(got), err)
	}
}
