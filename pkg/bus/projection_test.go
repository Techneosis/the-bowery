package bus

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// rec is a record for the fold: the fold reads what a node already
// verified, so it needs the event alone.
func rec(producer string, seq uint64, at time.Time, kind, data string) Record {
	region, _, _ := strings.Cut(producer, "/")
	return Record{Event: Event{V: 1, Producer: producer, Seq: seq, Region: region, At: at, Kind: kind,
		Data: json.RawMessage(data)}}
}

var t0 = time.Date(2026, 11, 27, 3, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

// privateWorld is a little of everything: two producers, a card updated
// and closed, a request grouping shared work, a ticket, a hide and a note
// on a shared card, one removal, a shared record that the fold ignores and
// two private records it can't fold.
func privateWorld() []Record {
	return []Record{
		rec("north/alice", 1, at(1), "private.card", `{"key":"watching","subject":"Things I'm watching","summary":"three shared requests","refs":["key:umbrella","2026-11-26T105442-put-desk-862d"],"links":[{"title":"the plan","url":"https://example.com/plan"}]}`),
		rec("north/alice", 2, at(2), "private.request", `{"key":"mine","title":"My grouping","members":["key:umbrella","key:moving"]}`),
		rec("north/alice", 3, at(3), "private.ticket", `{"key":"t-1","title":"Look at the dashed edge","area":"ops","refs":["private/watching"]}`),
		rec("north/bob", 1, at(4), "private.card", `{"key":"watching","state":"done"}`),
		rec("north/alice", 4, at(5), "private.hide", `{"key":"2026-11-26T100745-put-desk-1efe"}`),
		rec("north/alice", 5, at(6), "private.note", `{"key":"n-1","on":"key:moving","text":"slow on the gate"}`),
		rec("north/alice", 6, at(7), "card.answered", `{"key":"private/watching"}`),
		rec("north/alice", 7, at(8), "private.request", `{"key":"sub","title":"Under mine","under":"private/mine"}`),
		rec("north/alice", 8, at(9), "private.card", `{"key":"gone","subject":"soon gone"}`),
		rec("north/bob", 2, at(10), "private.card", `{"key":"gone","remove":true}`),
		rec("north/alice", 9, at(11), "private.note", `{"n":1}`),
		rec("north/alice", 10, at(12), "private.request.opened", `{}`),
	}
}

func snapshot(t *testing.T, recs []Record) PrivateState {
	t.Helper()
	f := NewPrivateFold()
	for _, r := range recs {
		f.Apply(r)
	}
	return f.State()
}

func bytesOf(t *testing.T, s PrivateState) string {
	t.Helper()
	b, err := s.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func entry(t *testing.T, s PrivateState, list, key string) map[string]any {
	t.Helper()
	for _, e := range s.Lists[list] {
		if e.Key == key {
			var v map[string]any
			if err := json.Unmarshal(e.V, &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	return nil
}

func TestPrivateFoldShape(t *testing.T) {
	s := snapshot(t, privateWorld())
	if s.V != 1 {
		t.Fatalf("v %d", s.V)
	}
	for _, l := range PrivateLists {
		if s.Lists[l] == nil {
			t.Fatalf("list %s missing: every list is always there", l)
		}
	}
	// The cursor covers every private record folded, skipped ones too,
	// and not the shared one.
	if s.Cursor != "north/alice=10,north/bob=2" {
		t.Fatalf("cursor %q", s.Cursor)
	}
	c := entry(t, s, "card", "private/watching")
	if c == nil {
		t.Fatal("no card private/watching")
	}
	// A later record merges into the card: its state changes, the rest stays.
	if c["id"] != "private/watching" || c["subject"] != "Things I'm watching" || c["state"] != "done" || c["private"] != true {
		t.Fatalf("card %v", c)
	}
	if c["from"] != "north/alice" || c["sent"] != at(1).Format(time.RFC3339) || c["updated"] != at(4).Format(time.RFC3339) {
		t.Fatalf("card's provenance %v", c)
	}
	if refs := fmt.Sprint(c["refs"]); refs != "[key:umbrella 2026-11-26T105442-put-desk-862d]" {
		t.Fatalf("refs %s", refs)
	}
	if entry(t, s, "card", "private/gone") != nil {
		t.Fatal("a removed card is still there")
	}
	r := entry(t, s, "request", "private/mine")
	if r == nil || r["k"] != "private/mine" || r["t"] != "My grouping" || r["o"] != true || r["state"] != "open" || r["nested"] != 1.0 {
		t.Fatalf("request %v", r)
	}
	if sub := entry(t, s, "request", "private/sub"); sub == nil || sub["under"] != "private/mine" {
		t.Fatalf("sub-request %v", sub)
	}
	tk := entry(t, s, "ticket", "private/t-1")
	if tk == nil || tk["t"] != "Look at the dashed edge" || tk["area"] != "ops" || tk["state"] != "open" {
		t.Fatalf("ticket %v", tk)
	}
	if h := entry(t, s, "hide", "2026-11-26T100745-put-desk-1efe"); h == nil {
		t.Fatalf("hide missing: %s", bytesOf(t, s))
	}
	if n := entry(t, s, "note", "private/n-1"); n == nil || n["on"] != "key:moving" || n["text"] != "slow on the gate" {
		t.Fatalf("note %v", n)
	}
	// The two it can't fold are named, with why.
	sk := string(s.Meta["skipped"])
	if !strings.Contains(sk, "north/alice:9") || !strings.Contains(sk, "north/alice:10") || !strings.Contains(sk, "private.request.opened") {
		t.Fatalf("skipped %s", sk)
	}
}

// Fold parity: the same records, replayed twice, in any order across
// producers and in any batches, fold to the same bytes.
func TestPrivateFoldParity(t *testing.T) {
	world := privateWorld()
	want := bytesOf(t, snapshot(t, world))
	if again := bytesOf(t, snapshot(t, world)); again != want {
		t.Fatalf("replay twice differs:\n%s\n%s", want, again)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		// A node keeps each producer's order; across producers, any.
		var a, h []Record
		for _, r := range world {
			if r.Event.Producer == "north/alice" {
				a = append(a, r)
			} else {
				h = append(h, r)
			}
		}
		var mixed []Record
		for len(a)+len(h) > 0 {
			if len(h) == 0 || (len(a) > 0 && rng.Intn(2) == 0) {
				mixed, a = append(mixed, a[0]), a[1:]
			} else {
				mixed, h = append(mixed, h[0]), h[1:]
			}
		}
		// A record seen twice (a reader resuming) changes nothing.
		mixed = append(mixed, mixed[rng.Intn(len(mixed))])
		f := NewPrivateFold()
		for _, r := range mixed {
			f.Apply(r)
			if rng.Intn(3) == 0 {
				f.State() // a snapshot mid-way changes nothing either
			}
		}
		if got := bytesOf(t, f.State()); got != want {
			t.Fatalf("order %d differs:\n%s\n%s", i, want, got)
		}
	}
}

func TestPrivateFoldIgnoresShared(t *testing.T) {
	s := snapshot(t, []Record{rec("north/alice", 1, at(1), "card.answered", `{"key":"private/x","subject":"x"}`)})
	if s.Cursor != "" {
		t.Fatalf("cursor %q", s.Cursor)
	}
	for _, l := range PrivateLists {
		if len(s.Lists[l]) != 0 {
			t.Fatalf("a shared record reached the private %s list", l)
		}
	}
}

func TestCheckPrivate(t *testing.T) {
	for _, c := range []struct {
		kind, data, err string
	}{
		{"private.card", `{"subject":"x"}`, ""},
		{"private.card", `{"key":"a.b-c_1","subject":"x","kind":"fyi","steps":["one"],"links":[{"title":"t","url":"https://x"}]}`, ""},
		{"private.card", `{"key":"private/a","subject":"x"}`, ""},
		{"private.card", `{"key":"watching","state":"done"}`, ""}, // an update: subject's from before
		{"private.card", `{"key":"gone","remove":true}`, ""},
		{"private.card", `{}`, "subject"},
		{"private.card", `{"remove":true}`, "key"},
		{"private.card", `{"subject":"x","links":[{"title":"t","url":"javascript:alert(1)"}]}`, "http"},
		{"private.card", `{"subject":"x","kind":"wat"}`, "kind"},
		{"private.card", `{"subject":"x","nope":1}`, "nope"},
		{"private.card", `{"key":"Has Space","subject":"x"}`, "key"},
		{"private.card", ``, "data"},
		{"private.request", `{"title":"x","under":"key:umbrella","members":["key:a"]}`, ""},
		{"private.request", `{"members":["key:a"]}`, "title"},
		{"private.request", `{"title":"x","state":"wat"}`, "state"},
		{"private.ticket", `{"title":"x","area":"ops"}`, ""},
		{"private.hide", `{"key":"2026-11-26T100745-put-desk-1efe"}`, ""},
		{"private.hide", `{"key":"k","undo":true}`, ""},
		{"private.hide", `{}`, "key"},
		{"private.note", `{"on":"key:a","text":"t"}`, ""},
		{"private.note", `{"on":"key:a"}`, "text"},
		{"private.note", `{"n":1}`, "n"},
		{"private.thing", `{}`, "private.card"},
		{"card.answered", `{}`, "private"},
	} {
		err := CheckPrivate(c.kind, json.RawMessage(c.data))
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s %s: %v", c.kind, c.data, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s %s: want an error naming %q, got %v", c.kind, c.data, c.err, err)
		}
	}
}

// Private may name shared; shared never names private. A node refuses a
// shared topic whose event names a private key anywhere in it.
func TestSharedNeverNamesPrivate(t *testing.T) {
	n, _ := newNode(t, 0)
	if _, err := n.Publish("alice", Publication{Kind: "private.card", Data: json.RawMessage(`{"subject":"x","refs":["key:shared"]}`)}); err != nil {
		t.Fatalf("a private record naming a shared key: %v", err)
	}
	for _, p := range []Publication{
		{Kind: "job.started", Data: json.RawMessage(`{"refs":["private/watching"]}`)},
		{Kind: "job.started", Subject: json.RawMessage(`"private/watching"`)},
		{Kind: "job.started", Actor: json.RawMessage(`{"a":{"b":["x","private/w"]}}`)},
	} {
		_, err := n.Publish("alice", p)
		if err == nil || !strings.Contains(err.Error(), "private") {
			t.Fatalf("a shared record naming a private key was published (err %v)", err)
		}
	}
	if _, err := n.Publish("alice", Publication{Kind: "job.started", Data: json.RawMessage(`{"path":"a/private/b","note":"private work"}`)}); err != nil {
		t.Fatalf("a shared record merely saying private: %v", err)
	}
	if !NamesPrivate(json.RawMessage(`["x",{"y":"private/z"}]`)) || NamesPrivate(json.RawMessage(`{"private/z":1}`)) == false {
		t.Fatal("NamesPrivate misses a private key in a value or a key")
	}
}
