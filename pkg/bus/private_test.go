package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
)

// private.* is private in every region, whether its [bus] private says so
// or not: a region file that forgets the line still never sends a private
// card to a peer.
func TestPrivateIsAlwaysPrivate(t *testing.T) {
	r := RulesOf(region(t, `
[peers.south]
address     = "/srv/south"
may_publish = ["*"]
may_read    = ["*", "private.*"]
`))
	for _, k := range []string{"private.card", "private.request.opened"} {
		if !r.Private(k) {
			t.Errorf("%s isn't private with no [bus] private", k)
		}
		if r.Allowed("south", k, Read) || r.Allowed("south", k, Publish) {
			t.Errorf("%s may go to or come from a peer", k)
		}
	}
	if r.Private("privates.card") || !r.Allowed("south", "card.answered", Read) {
		t.Fatal("the floor covers more than private.*")
	}
	// The floor is there for a Rules built by hand, too.
	if !(Rules{}).Private("private.note") {
		t.Fatal("an empty Rules lets private.* out")
	}
}

// The property the design's private work rests on: whatever a peer's
// lists say, and whichever way replication runs (pushed by this node, or
// pulled from it), no private.* event ever reaches the peer.
func TestNoPeerSetEverGetsPrivate(t *testing.T) {
	rng := rand.New(rand.NewSource(20261127))
	pats := []string{"*", "private.*", "private.card", "private", "job.*", "card.*", "seat.*", "gate.*", "p*"}
	kinds := []string{"private.card", "private.request", "private.ticket.opened", "job.started", "card.answered", "seat.note", "gate.asked"}
	pick := func() Patterns {
		var out Patterns
		for _, p := range pats {
			if rng.Intn(2) == 0 {
				out = append(out, p)
			}
		}
		return out
	}
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		nk, sk := key(t), key(t)
		seats := map[string]Topics{"alice": {Publish: Patterns{"*"}, Read: Patterns{"*"}}}
		var private Patterns
		if rng.Intn(2) == 0 {
			private = Patterns{"private.*"} // the region file says it, or doesn't
		}
		north := peerNode(t, "north", nk, map[string]PublicKey{"south": sk.Public}, Rules{
			Seats: seats, PrivateTopics: private,
			Peers: map[string]Topics{"south": {Publish: pick(), Read: pick()}},
		})
		south := peerNode(t, "south", sk, map[string]PublicKey{"north": nk.Public}, Rules{
			Seats: seats,
			Peers: map[string]Topics{"north": {Publish: Patterns{"*"}, Read: Patterns{"*"}}},
		})
		for j := 0; j < 12; j++ {
			if _, err := north.Publish("alice", pub(kinds[rng.Intn(len(kinds))])); err != nil {
				t.Fatal(err)
			}
		}
		d := t.TempDir()
		push := Peering{Peer: "south", Link: nodeLink{south}, StateFile: d + "/push.json"}
		pull := Peering{Peer: "north", Link: nodeLink{north}, StateFile: d + "/pull.json"}
		for k := 0; k < 3; k++ {
			if _, err := north.PushOnce(ctx, push); err != nil {
				t.Fatal(err)
			}
			if _, err := south.PullOnce(ctx, pull, 0); err != nil {
				t.Fatal(err)
			}
		}
		for _, k := range kindsOf(t, south) {
			if strings.HasPrefix(k, "private.") {
				t.Fatalf("round %d: south got %s", i, k)
			}
		}
	}
}

func rawMsgs(lines []string) []json.RawMessage {
	out := make([]json.RawMessage, len(lines))
	for i, l := range lines {
		out[i] = json.RawMessage(l)
	}
	return out
}

func kindsOf(t *testing.T, n *Node) []string {
	t.Helper()
	recs, err := n.Log().Since(Cursor{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		out = append(out, r.Event.Kind)
	}
	return out
}

// A peer that sends a private topic anyway (its node is old, or its rules
// were widened by hand) is refused, and the receiving node says so in its
// own log, not only in the reply the sender may never read.
func TestPrivateArrivalIsRefusedAndLogged(t *testing.T) {
	nk, sk := key(t), key(t)
	seats := map[string]Topics{"alice": {Publish: Patterns{"*"}, Read: Patterns{"*"}}}
	// north is careless: it would send south anything. (Rules can't say
	// that since the floor, so the test sends south a hand-made push.)
	north := peerNode(t, "north", nk, map[string]PublicKey{"south": sk.Public}, Rules{Seats: seats})
	var mu sync.Mutex
	var logged []string
	south, err := NewNode(NodeConfig{Dir: t.TempDir(), Region: "south", Sign: sk, Seal: sk,
		Peers: PeerKeys{"north": nk.Public},
		Rules: Rules{Seats: seats, Peers: map[string]Topics{"north": {Publish: Patterns{"*"}, Read: Patterns{"*"}}}},
		Logf: func(f string, a ...any) {
			mu.Lock()
			logged = append(logged, fmt.Sprintf(f, a...))
			mu.Unlock()
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { south.Close() })
	r, err := north.Publish("alice", pub("private.card"))
	if err != nil {
		t.Fatal(err)
	}
	ok, err := north.Publish("alice", pub("job.started"))
	if err != nil {
		t.Fatal(err)
	}
	var raws []string
	for _, rec := range []Record{r, ok} {
		line, err := rec.Line()
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, strings.TrimSuffix(string(line), "\n"))
	}
	env, err := north.sign(PeerCall{Op: "push", To: "south", Records: rawMsgs(raws)})
	if err != nil {
		t.Fatal(err)
	}
	rep := south.HandlePeer(context.Background(), env)
	if rep.Accepted != 1 || rep.Refused != 1 {
		t.Fatalf("push reply %+v", rep)
	}
	if got := strings.Join(kindsOf(t, south), ","); got != "job.started" {
		t.Fatalf("south kept %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 1 || !strings.Contains(logged[0], "private") || !strings.Contains(logged[0], "north") || !strings.Contains(logged[0], "private.card") {
		t.Fatalf("logged %q", logged)
	}
}
