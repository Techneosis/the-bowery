package bus

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two regions, north and south, each a node with its own region key. north
// lets south read job.* and card.* (private.* never leaves) and publish
// card.*; south lets north read everything and publish job.* alone, so
// north's card.note is one south refuses on arrival though north sends it.
type pair struct {
	north, south   *Node
	nkey, skey     PrivateKey
	nState, sState string
}

func peerNode(t *testing.T, region string, k PrivateKey, peers map[string]PublicKey, rules Rules) *Node {
	t.Helper()
	n, err := NewNode(NodeConfig{Dir: t.TempDir(), Region: region, Rules: rules, Sign: k, Seal: k, Peers: peers})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func newPair(t *testing.T) pair {
	nk, sk := key(t), key(t)
	seats := map[string]Topics{"alice": {Publish: Patterns{"*"}, Read: Patterns{"*"}}}
	north := peerNode(t, "north", nk, map[string]PublicKey{"south": sk.Public}, Rules{
		Seats:         seats,
		PrivateTopics: Patterns{"private.*"},
		Peers:         map[string]Topics{"south": {Publish: Patterns{"card.*"}, Read: Patterns{"job.*", "card.*", "private.*"}}},
	})
	south := peerNode(t, "south", sk, map[string]PublicKey{"north": nk.Public}, Rules{
		Seats: seats,
		Peers: map[string]Topics{"north": {Publish: Patterns{"job.*"}, Read: Patterns{"*"}}},
	})
	d := t.TempDir()
	return pair{north: north, south: south, nkey: nk, skey: sk,
		nState: filepath.Join(d, "north-south.json"), sState: filepath.Join(d, "south-north.json")}
}

// nodeLink calls another node in-process, as a transport would.
type nodeLink struct{ n *Node }

func (l nodeLink) Call(ctx context.Context, env PeerEnvelope) (PeerReply, error) {
	return l.n.HandlePeer(ctx, env), nil
}

func (p pair) northToSouth() Peering {
	return Peering{Peer: "south", Link: nodeLink{p.south}, StateFile: p.nState}
}

func kinds(t *testing.T, n *Node, producer string) []string {
	t.Helper()
	recs, err := n.Log().Since(Cursor{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range recs {
		if r.Event.Producer == producer {
			out = append(out, r.Event.Kind)
		}
	}
	return out
}

func TestPushSendsOnlyWhatBothSidesAllow(t *testing.T) {
	p := newPair(t)
	for _, k := range []string{"job.started", "private.note", "card.note", "gate.asked", "job.done"} {
		if _, err := p.north.Publish("alice", pub(k)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	res, err := p.north.PushOnce(ctx, p.northToSouth())
	if err != nil {
		t.Fatal(err)
	}
	// Sent: what south may read from north (private never; gate.* isn't
	// granted). Refused on arrival: card.note, which south's rules don't
	// let north publish.
	if res.Sent != 3 || res.Accepted != 2 || res.Refused != 1 {
		t.Fatalf("push %+v", res)
	}
	if got := strings.Join(kinds(t, p.south, "north/alice"), ","); got != "job.started,job.done" {
		t.Fatalf("south has %s", got)
	}
	// The records south keeps verify as north's, with north's seal.
	recs, _ := p.south.Log().Since(Cursor{}, 0)
	for _, r := range recs {
		if err := r.Verify(nodeKeys{region: "south", sign: p.skey.Public, seal: p.skey.Public, more: PeerKeys{"north": p.nkey.Public}}); err != nil {
			t.Fatal(err)
		}
	}
	// The cursor is kept: a second push sends nothing, and so does a new
	// replicator reading the same state file (a restart).
	if res, err := p.north.PushOnce(ctx, p.northToSouth()); err != nil || res.Sent != 0 {
		t.Fatalf("second push %+v %v", res, err)
	}
	n2 := p.northToSouth()
	st, err := LoadPeerState(n2.StateFile)
	if err != nil || st.Pushed != "north/alice=5" {
		t.Fatalf("state %+v %v", st, err)
	}
	p.north.Publish("alice", pub("job.done"))
	if res, err := p.north.PushOnce(ctx, n2); err != nil || res.Sent != 1 || res.Accepted != 1 {
		t.Fatalf("after restart %+v %v", res, err)
	}
}

func TestPullTakesOnlyWhatBothSidesAllow(t *testing.T) {
	p := newPair(t)
	for _, k := range []string{"card.answered", "job.started", "gate.decided"} {
		if _, err := p.south.Publish("alice", pub(k)); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	res, err := p.north.PullOnce(ctx, p.northToSouth(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// south lets north read everything; north takes card.* alone from south.
	if res.Received != 3 || res.Accepted != 1 || res.Refused != 2 {
		t.Fatalf("pull %+v", res)
	}
	if got := strings.Join(kinds(t, p.north, "south/alice"), ","); got != "card.answered" {
		t.Fatalf("north has %s", got)
	}
	st, _ := LoadPeerState(p.nState)
	if st.Pulled != "south/alice=3" {
		t.Fatalf("pulled cursor %q", st.Pulled)
	}
	if res, err := p.north.PullOnce(ctx, p.northToSouth(), 0); err != nil || res.Received != 0 {
		t.Fatalf("second pull %+v %v", res, err)
	}
}

func TestPullDoesNotEchoThePullersOwnEvents(t *testing.T) {
	p := newPair(t)
	p.north.Publish("alice", pub("job.started"))
	ctx := context.Background()
	if _, err := p.north.PushOnce(ctx, p.northToSouth()); err != nil {
		t.Fatal(err)
	}
	if res, err := p.north.PullOnce(ctx, p.northToSouth(), 0); err != nil || res.Received != 0 {
		t.Fatalf("pull %+v %v", res, err)
	}
}

func TestPullLongPollsUntilSomethingArrives(t *testing.T) {
	p := newPair(t)
	go func() {
		time.Sleep(150 * time.Millisecond)
		p.south.Publish("alice", pub("card.answered"))
	}()
	start := time.Now()
	res, err := p.north.PullOnce(context.Background(), p.northToSouth(), 5*time.Second)
	if err != nil || res.Accepted != 1 {
		t.Fatalf("pull %+v %v", res, err)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Fatalf("the long-poll answered after %v", d)
	}
}

func TestReplicateRunsBothWays(t *testing.T) {
	p := newPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		p.north.Replicate(ctx, Peering{Peer: "south", Link: nodeLink{p.south}, StateFile: p.nState,
			Push: true, Pull: true, Wait: time.Second})
	}()
	p.north.Publish("alice", pub("job.started"))
	p.south.Publish("alice", pub("card.answered"))
	deadline := time.Now().Add(5 * time.Second)
	var st []PeerState
	for time.Now().Before(deadline) {
		st = p.north.PeerStatus()
		// Each cursor moves past the other side's events too, once it has
		// looked again: north never sends south its own, nor south north.
		both := "north/alice=1,south/alice=1"
		if len(kinds(t, p.south, "north/alice")) == 1 && len(kinds(t, p.north, "south/alice")) == 1 &&
			len(st) == 1 && st[0].Peer == "south" && st[0].Pushed == both && st[0].Pulled == both {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("not replicated: south %v, north %v, status %+v", kinds(t, p.south, "north/alice"), kinds(t, p.north, "south/alice"), st)
}

func call(t *testing.T, c PeerCall, k PrivateKey) PeerEnvelope {
	t.Helper()
	env, err := SignPeerCall(c, k)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestHandlePeerRefusesStrangersAndReplays(t *testing.T) {
	p := newPair(t)
	p.south.Publish("alice", pub("card.answered"))
	ctx := context.Background()
	now := time.Now().UTC()
	pull := func(from, to string, at time.Time) PeerCall {
		return PeerCall{Op: "pull", From: from, To: to, At: at, Nonce: newNonce()}
	}
	other := key(t)
	for _, c := range []struct {
		name string
		env  PeerEnvelope
		want string
	}{
		{"unknown peer", call(t, pull("west", "south", now), other), "isn't a peer"},
		{"wrong key", call(t, pull("north", "south", now), other), "isn't north's"},
		{"another region", call(t, pull("north", "east", now), p.nkey), "for region east"},
		{"stale", call(t, pull("north", "south", now.Add(-10*time.Minute)), p.nkey), "clock"},
		{"future", call(t, pull("north", "south", now.Add(10*time.Minute)), p.nkey), "clock"},
		{"bad op", call(t, PeerCall{Op: "drop", From: "north", To: "south", At: now, Nonce: newNonce()}, p.nkey), "unknown peer op"},
		{"tampered", func() PeerEnvelope {
			e := call(t, pull("north", "south", now), p.nkey)
			e.Call = json.RawMessage(strings.Replace(string(e.Call), `"pull"`, `"push"`, 1))
			return e
		}(), "signature"},
	} {
		if r := p.south.HandlePeer(ctx, c.env); !strings.Contains(r.Error, c.want) {
			t.Errorf("%s: %q, want %q", c.name, r.Error, c.want)
		}
	}
	env := call(t, pull("north", "south", now), p.nkey)
	if r := p.south.HandlePeer(ctx, env); r.Error != "" || len(r.Records) != 1 {
		t.Fatalf("first %+v", r)
	}
	if r := p.south.HandlePeer(ctx, env); !strings.Contains(r.Error, "seen") {
		t.Fatalf("replay %+v", r)
	}
}

func TestPushedRecordsMustVerify(t *testing.T) {
	p := newPair(t)
	// A record north's key never signed, for a producer north speaks for.
	other := key(t)
	r, err := Sign(Event{V: 1, Producer: "north/alice", Seq: 1, Region: "north", At: time.Now().UTC(), Kind: "job.started"}, other, other)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := r.Line()
	env := call(t, PeerCall{Op: "push", From: "north", To: "south", At: time.Now().UTC(), Nonce: newNonce(),
		Records: []json.RawMessage{json.RawMessage(strings.TrimSpace(string(line)))}}, p.nkey)
	rep := p.south.HandlePeer(context.Background(), env)
	if rep.Error != "" || rep.Accepted != 0 || rep.Refused != 1 || len(rep.Problems) != 1 {
		t.Fatalf("%+v", rep)
	}
	if len(kinds(t, p.south, "north/alice")) != 0 {
		t.Fatal("south kept a forged record")
	}
}
