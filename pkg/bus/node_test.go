package bus

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// nodeWorld is a node for region north whose local seats are alice (job.*
// and private.* both ways, everything to read) and bob (reads card.* only).
func newNode(t *testing.T, buffer int) (*Node, world) {
	t.Helper()
	w := world{seat: key(t), region: key(t)}
	n, err := NewNode(NodeConfig{
		Dir:    t.TempDir(),
		Region: "north",
		Rules: Rules{
			Seats: map[string]Topics{
				"alice": {Publish: Patterns{"job.*", "private.*"}, Read: Patterns{"*"}},
				"bob":   {Read: Patterns{"card.*"}},
			},
			PrivateTopics: Patterns{"private.*"},
		},
		Sign:   w.seat,
		Seal:   w.region,
		Buffer: buffer,
		Stall:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	w.keys = keys{
		producers: map[string]PublicKey{"north/alice": w.seat.Public, "north/bob": w.seat.Public},
		regions:   map[string]PublicKey{"north": w.region.Public},
	}
	return n, w
}

func pub(kind string) Publication {
	return Publication{Kind: kind, Owners: []string{"alice"}, Subject: json.RawMessage(`{"job":"j-1"}`)}
}

func TestNodePublishSignsAsTheSeatAndSeals(t *testing.T) {
	n, w := newNode(t, 0)
	r1, err := n.Publish("alice", pub("job.started"))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := n.Publish("alice", pub("job.done"))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Ref() != (Ref{"north/alice", 1}) || r2.Ref() != (Ref{"north/alice", 2}) {
		t.Fatalf("refs %v %v", r1.Ref(), r2.Ref())
	}
	for _, r := range []Record{r1, r2} {
		if err := r.Verify(w.keys); err != nil {
			t.Fatalf("%v: %v", r.Ref(), err)
		}
	}
	if h := n.Log().Heads(); h["north/alice"] != 2 {
		t.Fatalf("heads %v", h)
	}
	for _, c := range []struct {
		seat, kind, want string
	}{
		{"alice", "card.answered", "may not publish card.answered"},
		{"bob", "job.started", "may not publish job.started"},
		{"carol", "job.started", "may not publish job.started"},
		{"Carol", "job.started", "isn't a seat name"},
		{"alice", "job", "isn't a topic"},
	} {
		if _, err := n.Publish(c.seat, pub(c.kind)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s publishing %s: %v, want %q", c.seat, c.kind, err, c.want)
		}
	}
}

// client is one connection to the node, the other end served by Serve.
type client struct {
	t  *testing.T
	c  net.Conn
	rd *bufio.Reader
}

func dial(t *testing.T, n *Node, seat string) *client {
	t.Helper()
	a, b := net.Pipe()
	go n.Serve(b, seat)
	t.Cleanup(func() { a.Close() })
	return &client{t: t, c: a, rd: bufio.NewReader(a)}
}

func (c *client) send(req Request) {
	c.t.Helper()
	b, _ := json.Marshal(req)
	c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.c.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) next() Reply {
	c.t.Helper()
	c.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.rd.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("reading a reply: %v", err)
	}
	var r Reply
	if err := json.Unmarshal(line, &r); err != nil {
		c.t.Fatalf("%s: %v", line, err)
	}
	return r
}

func (r Reply) record(t *testing.T) Record {
	t.Helper()
	if r.Record == nil {
		t.Fatalf("not a record: %+v", r)
	}
	rec, err := ParseLine(*r.Record)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestNodeServePublishAndStatus(t *testing.T) {
	n, w := newNode(t, 0)
	c := dial(t, n, "alice")
	p := pub("job.started")
	c.send(Request{Op: "publish", Publication: &p})
	r := c.next()
	if r.Error != "" || r.Ref == nil || *r.Ref != (Ref{"north/alice", 1}) {
		t.Fatalf("publish: %+v", r)
	}
	if err := r.record(t).Verify(w.keys); err != nil {
		t.Fatal(err)
	}
	c = dial(t, n, "alice")
	c.send(Request{Op: "status"})
	r = c.next()
	if r.Status == nil || r.Status.Seat != "alice" || r.Status.Region != "north" || r.Status.Heads != "north/alice=1" ||
		strings.Join(r.Status.MayPublish, ",") != "job.*,private.*" || strings.Join(r.Status.MayRead, ",") != "*" {
		t.Fatalf("status: %+v", r.Status)
	}
	c = dial(t, n, "bob")
	c.send(Request{Op: "publish", Publication: &p})
	if r := c.next(); !strings.Contains(r.Error, "may not publish job.started") {
		t.Fatalf("bob publishing: %+v", r)
	}
	c = dial(t, n, "alice")
	c.send(Request{Op: "nonsense"})
	if r := c.next(); !strings.Contains(r.Error, "nonsense") {
		t.Fatalf("an unknown op: %+v", r)
	}
}

func TestNodeAsIsDevModeOnly(t *testing.T) {
	n, _ := newNode(t, 0)
	p := pub("job.started")
	c := dial(t, n, "bob")
	c.send(Request{Op: "publish", As: "alice", Publication: &p})
	if r := c.next(); !strings.Contains(r.Error, "dev mode") {
		t.Fatalf("as outside dev mode: %+v", r)
	}
	n.cfg.Dev = true
	c = dial(t, n, "bob")
	c.send(Request{Op: "publish", As: "alice", Publication: &p})
	if r := c.next(); r.Error != "" || r.Ref.Producer != "north/alice" {
		t.Fatalf("as in dev mode: %+v", r)
	}
}

func TestNodeFollowStreamsWhatTheSeatMayRead(t *testing.T) {
	n, _ := newNode(t, 0)
	for _, k := range []string{"job.started", "private.note", "job.done"} {
		if _, err := n.Publish("alice", pub(k)); err != nil {
			t.Fatal(err)
		}
	}
	// alice reads everything, from the start, then is caught up.
	c := dial(t, n, "alice")
	c.send(Request{Op: "follow"})
	for _, want := range []uint64{1, 2, 3} {
		if got := c.next().record(t).Event.Seq; got != want {
			t.Fatalf("seq %d, want %d", got, want)
		}
	}
	if r := c.next(); !r.CaughtUp || r.Cursor != "north/alice=3" {
		t.Fatalf("caught up: %+v", r)
	}
	// A new event wakes the follower at once (well inside the backstop).
	start := time.Now()
	if _, err := n.Publish("alice", pub("job.started")); err != nil {
		t.Fatal(err)
	}
	if got := c.next().record(t).Event.Seq; got != 4 {
		t.Fatalf("seq %d after the wake", got)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("the wake took %v", d)
	}

	// A cursor and a topic filter: only what's past it, of those topics.
	c = dial(t, n, "alice")
	c.send(Request{Op: "follow", Cursor: "north/alice=1", Topics: []string{"job.*"}, Once: true})
	for _, want := range []uint64{3, 4} {
		if got := c.next().record(t).Event.Seq; got != want {
			t.Fatalf("filtered seq %d, want %d", got, want)
		}
	}
	if r := c.next(); !r.CaughtUp || r.Cursor != "north/alice=4" {
		t.Fatalf("caught up: %+v", r)
	}
	if _, err := c.rd.ReadByte(); err == nil {
		t.Fatal("--once left the connection open")
	}

	// bob may read card.* only: none of alice's job or private events.
	c = dial(t, n, "bob")
	c.send(Request{Op: "follow", Once: true})
	if r := c.next(); !r.CaughtUp || r.Record != nil {
		t.Fatalf("bob: %+v", r)
	}

	// A topic outside the seat's own read list widens nothing.
	c = dial(t, n, "bob")
	c.send(Request{Op: "follow", Topics: []string{"job.*"}, Once: true})
	if r := c.next(); !r.CaughtUp || r.Record != nil {
		t.Fatalf("bob asking for job.*: %+v", r)
	}

	c = dial(t, n, "alice")
	c.send(Request{Op: "follow", Cursor: "nope"})
	if r := c.next(); !strings.Contains(r.Error, "cursor") {
		t.Fatalf("a bad cursor: %+v", r)
	}
	c = dial(t, n, "alice")
	c.send(Request{Op: "follow", Topics: []string{"job*"}})
	if r := c.next(); !strings.Contains(r.Error, "job*") {
		t.Fatalf("a bad topic: %+v", r)
	}
}

func TestNodeFollowBackPressure(t *testing.T) {
	n, _ := newNode(t, 4)
	const total = 12
	for i := 0; i < total; i++ {
		if _, err := n.Publish("alice", pub("job.started")); err != nil {
			t.Fatal(err)
		}
	}
	c := dial(t, n, "alice")
	c.send(Request{Op: "follow"})
	time.Sleep(200 * time.Millisecond) // a reader that's fallen behind
	var got []uint64
	var end Reply
	for {
		r := c.next()
		if r.End != "" {
			end = r
			break
		}
		got = append(got, r.record(t).Event.Seq)
	}
	if len(got) >= total {
		t.Fatalf("no back-pressure: %d records", len(got))
	}
	want := Cursor{}
	if len(got) > 0 {
		want["north/alice"] = got[len(got)-1]
	}
	if end.Cursor != want.String() || end.End != "resume from "+want.String() {
		t.Fatalf("end %+v after %v", end, got)
	}
	if _, err := c.rd.ReadByte(); err == nil {
		t.Fatal("the node didn't close the connection")
	}
	// Resuming from the cursor reads the rest, nothing twice.
	c = dial(t, n, "alice")
	c.send(Request{Op: "follow", Cursor: end.Cursor})
	for {
		r := c.next()
		if r.CaughtUp {
			break
		}
		got = append(got, r.record(t).Event.Seq)
	}
	if len(got) != total {
		t.Fatalf("after resume: %v", got)
	}
	for i, s := range got {
		if s != uint64(i+1) {
			t.Fatalf("after resume: %v", got)
		}
	}
}

func TestNodeDefaultBuffer(t *testing.T) {
	n, _ := newNode(t, 0)
	if n.cfg.Buffer != 1024 {
		t.Fatalf("buffer %d", n.cfg.Buffer)
	}
	d, err := NewNode(NodeConfig{Dir: t.TempDir(), Region: "north", Sign: key(t), Seal: key(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.cfg.Stall != 5*time.Second {
		t.Fatalf("stall %v", d.cfg.Stall)
	}
	// One node per directory.
	if _, err := NewNode(NodeConfig{Dir: n.cfg.Dir, Region: "north", Sign: key(t), Seal: key(t)}); err == nil ||
		!strings.Contains(err.Error(), "another node") {
		t.Fatalf("a second node: %v", err)
	}
}

func TestNodeFollowTooOld(t *testing.T) {
	n, _ := newNode(t, 0)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	n.cfg.Now = func() time.Time { return old }
	for i := 0; i < 3; i++ {
		if _, err := n.Publish("alice", pub("job.started")); err != nil {
			t.Fatal(err)
		}
	}
	n.cfg.Now = time.Now
	if d, err := n.Compact(); err != nil || d != 2 {
		t.Fatalf("compact: %d, %v", d, err)
	}
	c := dial(t, n, "alice")
	c.send(Request{Op: "follow"})
	if r := c.next(); !strings.Contains(r.Error, "older than the log keeps") || r.Floors != "north/alice=2" {
		t.Fatalf("too old: %+v", r)
	}
	c = dial(t, n, "alice")
	c.send(Request{Op: "follow", Cursor: "north/alice=2", Once: true})
	if got := c.next().record(t).Event.Seq; got != 3 {
		t.Fatalf("from the floor: seq %d", got)
	}
}
