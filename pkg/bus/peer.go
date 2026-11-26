package bus

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Techneosis/the-bowery/internal/offices"
)

// Replication: a node exchanges events with each peer region the region
// file names. Two calls carry everything, both made by the side that can
// reach the other (a machine behind a firewall makes both, and its peer
// only answers):
//
//   - push: here are records you may read from me;
//   - pull: send me what I may read from you, past this cursor (a
//     long-poll: it waits for something new, up to its wait).
//
// Every call is signed with the calling region's key (namespace bus-peer)
// and names both regions and its time, so the answering node knows which
// peer is asking without trusting the transport, and refuses a call it has
// seen before. The topic rules are checked on both sides: the sender sends
// only what the peer may read (never a private topic), and the receiver
// keeps only what that peer may publish into it, after verifying each
// record's own signature and seal against its pinned keys.

// NSPeer is the namespace a region signs its peer calls in.
const NSPeer = "bus-peer"

const (
	// PeerSkew is how far a call's time may be from the answering node's.
	PeerSkew = 5 * time.Minute
	// MaxBatch is the most records one call carries.
	MaxBatch = 256
	// MaxPeerCall caps a call's bytes on the wire (a full push batch).
	MaxPeerCall = 8 << 20
	// MaxWait is the longest a pull may wait for something new.
	MaxWait = 20 * time.Second
)

// PeerKeys pins each peer region's key: it seals the region's events and
// signs for its seats (a node signs its own seats' events with its region
// key).
type PeerKeys map[string]PublicKey

func (k PeerKeys) ProducerKeys(p string) []PublicKey {
	if reg, _, ok := strings.Cut(p, "/"); ok {
		if key, ok := k[reg]; ok {
			return []PublicKey{key}
		}
	}
	return nil
}

func (k PeerKeys) RegionKeys(r string) []PublicKey {
	if key, ok := k[r]; ok {
		return []PublicKey{key}
	}
	return nil
}

// PeerCall is one call from a peer region.
type PeerCall struct {
	Op    string    `json:"op"` // push or pull
	From  string    `json:"from"`
	To    string    `json:"to"`
	At    time.Time `json:"at"`
	Nonce string    `json:"nonce"`
	// Push: the records, one Line each (no newline).
	Records []json.RawMessage `json:"records,omitempty"`
	// Pull: from Cursor (empty is the start), at most Limit records,
	// waiting up to WaitMS for one.
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	WaitMS int64  `json:"wait_ms,omitempty"`
}

// PeerEnvelope is a call as sent: its exact bytes and the signature over
// them.
type PeerEnvelope struct {
	Call json.RawMessage `json:"call"`
	Sig  string          `json:"sig"`
}

// PeerReply is the answering node's one reply.
type PeerReply struct {
	Error string `json:"error,omitempty"`
	// Push: what was kept, what it had already, and what it refused (with
	// the first few reasons).
	Accepted   int      `json:"accepted,omitempty"`
	Duplicates int      `json:"duplicates,omitempty"`
	Refused    int      `json:"refused,omitempty"`
	Problems   []string `json:"problems,omitempty"`
	// Pull: the records, and the cursor the puller has now read to (past
	// what it may not read too); More is a full batch, so ask again now.
	// Gap names the producers whose events retention dropped before the
	// puller had them.
	Records []json.RawMessage `json:"records,omitempty"`
	Cursor  string            `json:"cursor,omitempty"`
	More    bool              `json:"more,omitempty"`
	Gap     string            `json:"gap,omitempty"`
}

func newNonce() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// SignPeerCall signs a call with the calling region's key.
func SignPeerCall(c PeerCall, k PrivateKey) (PeerEnvelope, error) {
	if c.Nonce == "" {
		c.Nonce = newNonce()
	}
	b, err := json.Marshal(c)
	if err != nil {
		return PeerEnvelope{}, err
	}
	sig, err := offices.SignWith(k, NSPeer, b)
	if err != nil {
		return PeerEnvelope{}, err
	}
	return PeerEnvelope{Call: b, Sig: string(sig)}, nil
}

const maxProblems = 8

// HandlePeer answers one peer's call. Who is calling is the signature
// alone: the transport (a socket anyone local may open, an http endpoint)
// says nothing.
func (n *Node) HandlePeer(ctx context.Context, env PeerEnvelope) PeerReply {
	c, err := n.checkCall(env)
	if err != nil {
		return PeerReply{Error: err.Error()}
	}
	switch c.Op {
	case "push":
		return n.takePush(c)
	case "pull":
		return n.servePull(ctx, c)
	}
	return PeerReply{Error: fmt.Sprintf("unknown peer op %q: push or pull", c.Op)}
}

func (n *Node) checkCall(env PeerEnvelope) (PeerCall, error) {
	var c PeerCall
	dec := json.NewDecoder(bytes.NewReader(env.Call))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("the call isn't one: %v", err)
	}
	pin, ok := n.cfg.Peers[c.From]
	if !ok {
		return c, fmt.Errorf("%q isn't a peer of region %s", c.From, n.cfg.Region)
	}
	got, err := offices.VerifySignature([]byte(env.Sig), NSPeer, env.Call)
	if err != nil {
		return c, fmt.Errorf("the call's signature: %v", err)
	}
	if !got.Equal(pin) {
		return c, fmt.Errorf("the call is signed by %s, which isn't %s's key", got.Fingerprint(), c.From)
	}
	if c.To != n.cfg.Region {
		return c, fmt.Errorf("the call is for region %s, and this is %s", c.To, n.cfg.Region)
	}
	now := n.cfg.Now()
	if d := now.Sub(c.At); d > PeerSkew || d < -PeerSkew {
		return c, fmt.Errorf("the call was made at %s, and this node's clock says %s", c.At.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	// A call is answered once: a pull replayed within the window would
	// hand its records to whoever copied it.
	sum := sha256.Sum256([]byte(env.Sig))
	id := hex.EncodeToString(sum[:])
	n.seenMu.Lock()
	defer n.seenMu.Unlock()
	for k, t := range n.seen {
		if now.After(t) {
			delete(n.seen, k)
		}
	}
	if _, dup := n.seen[id]; dup {
		return c, errors.New("this call has been seen before")
	}
	if n.seen == nil {
		n.seen = map[string]time.Time{}
	}
	n.seen[id] = c.At.Add(2 * PeerSkew)
	return c, nil
}

func (n *Node) takePush(c PeerCall) PeerReply {
	if len(c.Records) > MaxBatch {
		return PeerReply{Error: fmt.Sprintf("a push carries at most %d records", MaxBatch)}
	}
	var rep PeerReply
	refuse := func(why string) {
		rep.Refused++
		if len(rep.Problems) < maxProblems {
			rep.Problems = append(rep.Problems, why)
		}
	}
	for _, raw := range c.Records {
		r, err := ParseLine(raw)
		if err != nil {
			refuse(err.Error())
			continue
		}
		if !n.cfg.Rules.Allowed(c.From, r.Event.Kind, Publish) {
			refuse(fmt.Sprintf("%v: %s may not publish %s here", r.Ref(), c.From, r.Event.Kind))
			continue
		}
		added, err := n.log.Append(r)
		switch {
		case err != nil:
			refuse(err.Error())
		case added:
			rep.Accepted++
		default:
			rep.Duplicates++
		}
	}
	if rep.Accepted > 0 {
		n.Wake()
	}
	return rep
}

func (n *Node) servePull(ctx context.Context, c PeerCall) PeerReply {
	cur, err := ParseCursor(c.Cursor)
	if err != nil {
		return PeerReply{Error: err.Error()}
	}
	limit := c.Limit
	if limit <= 0 || limit > MaxBatch {
		limit = MaxBatch
	}
	wait := time.Duration(c.WaitMS) * time.Millisecond
	if wait > MaxWait {
		wait = MaxWait
	}
	deadline := time.Now().Add(wait)
	var gap Cursor
	for {
		wake := n.waiter()
		// A replica has no snapshot to resync from: it reads on from what
		// is kept, and is told what it missed.
		for p, f := range n.log.Floors() {
			if cur[p] < f {
				if gap == nil {
					gap = Cursor{}
				}
				gap[p] = f
			}
		}
		recs, err := n.log.SinceKept(cur, limit)
		if err != nil {
			return PeerReply{Error: err.Error()}
		}
		var rep PeerReply
		for _, r := range recs {
			cur.Advance(r.Ref())
			// Its own events the peer has; private ones never leave.
			if r.Event.Region == c.From || !n.cfg.Rules.Allowed(c.From, r.Event.Kind, Read) {
				continue
			}
			line, err := r.Line()
			if err != nil {
				return PeerReply{Error: err.Error()}
			}
			rep.Records = append(rep.Records, json.RawMessage(bytes.TrimSuffix(line, []byte("\n"))))
		}
		rep.More = len(recs) == limit
		rep.Cursor = cur.String()
		if gap != nil {
			rep.Gap = gap.String()
		}
		if len(rep.Records) > 0 || rep.More || !time.Now().Before(deadline) {
			return rep
		}
		t := time.NewTimer(min(time.Until(deadline), Backstop))
		select {
		case <-wake:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return rep
		}
		t.Stop()
	}
}

// ---------------------------------------------------------------- the calling side

// Link carries a call to one peer's node and brings back its reply.
type Link interface {
	Call(ctx context.Context, env PeerEnvelope) (PeerReply, error)
}

// Peering is one peer this node replicates with.
type Peering struct {
	Peer string
	Link Link
	// StateFile keeps the cursors (what the peer has had from here, and
	// what this node has read from it), so a restart resumes.
	StateFile string
	// Push sends the peer what it may read; Pull asks it for what it may
	// publish here. Wait is a pull's long-poll (default MaxWait).
	Push, Pull bool
	Wait       time.Duration
	Logf       func(format string, a ...any)
}

// PeerState is a peering's cursors and its last word, as kept on disk.
type PeerState struct {
	Peer     string    `json:"peer"`
	Pushed   string    `json:"pushed"`
	Pulled   string    `json:"pulled"`
	PushedAt time.Time `json:"pushed_at,omitzero"`
	PulledAt time.Time `json:"pulled_at,omitzero"`
	Sent     int       `json:"sent"`
	Received int       `json:"received"`
	Refused  int       `json:"refused"`
	Gap      string    `json:"gap,omitempty"`
	Error    string    `json:"error,omitempty"`
	ErrorAt  time.Time `json:"error_at,omitzero"`
}

// LoadPeerState reads a peering's state; a missing file is a new peering.
func LoadPeerState(path string) (PeerState, error) {
	var s PeerState
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %v", path, err)
	}
	if _, err := ParseCursor(s.Pushed); err != nil {
		return s, fmt.Errorf("%s: pushed: %v", path, err)
	}
	if _, err := ParseCursor(s.Pulled); err != nil {
		return s, fmt.Errorf("%s: pulled: %v", path, err)
	}
	return s, nil
}

// peering is a peering's state in memory; push and pull share it.
type peering struct {
	mu   sync.Mutex
	path string
	st   PeerState
}

func (n *Node) peering(p Peering) (*peering, error) {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	if n.peers == nil {
		n.peers = map[string]*peering{}
	}
	if pg, ok := n.peers[p.Peer]; ok && pg.path == p.StateFile {
		return pg, nil
	}
	st, err := LoadPeerState(p.StateFile)
	if err != nil {
		return nil, err
	}
	st.Peer = p.Peer
	pg := &peering{path: p.StateFile, st: st}
	n.peers[p.Peer] = pg
	return pg, nil
}

// update changes the state and writes it down.
func (pg *peering) update(f func(*PeerState)) error {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	f(&pg.st)
	b, err := json.MarshalIndent(pg.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pg.path), 0o700); err != nil {
		return err
	}
	return offices.WriteAtomic(pg.path, append(b, '\n'), 0o600)
}

func (pg *peering) get() PeerState {
	pg.mu.Lock()
	defer pg.mu.Unlock()
	return pg.st
}

// PeerStatus is every peering's state, by peer.
func (n *Node) PeerStatus() []PeerState {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	out := make([]PeerState, 0, len(n.peers))
	for _, pg := range n.peers {
		out = append(out, pg.get())
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Peer < out[k].Peer })
	return out
}

// Result is what one push or pull did.
type Result struct {
	Sent, Received, Accepted, Duplicates, Refused int
	More                                          bool
}

func (n *Node) sign(c PeerCall) (PeerEnvelope, error) {
	c.From, c.At = n.cfg.Region, n.cfg.Now().UTC()
	return SignPeerCall(c, n.cfg.Seal)
}

func (pg *peering) failed(err error) {
	pg.update(func(s *PeerState) { s.Error, s.ErrorAt = err.Error(), time.Now().UTC() })
}

// PushOnce sends the peer the next batch it may read from this node's log,
// and moves the pushed cursor past everything it looked at.
func (n *Node) PushOnce(ctx context.Context, p Peering) (Result, error) {
	pg, err := n.peering(p)
	if err != nil {
		return Result{}, err
	}
	cur, _ := ParseCursor(pg.get().Pushed)
	// What retention dropped before the peer had it is gone for it too.
	recs, err := n.log.SinceKept(cur, MaxBatch)
	if err != nil {
		return Result{}, err
	}
	var res Result
	var batch []json.RawMessage
	size := 0
	for _, r := range recs {
		if r.Event.Region == p.Peer || !n.cfg.Rules.Allowed(p.Peer, r.Event.Kind, Read) {
			cur.Advance(r.Ref())
			continue
		}
		line, err := r.Line()
		if err != nil {
			return res, err
		}
		if size+len(line) > MaxPeerCall/2 && len(batch) > 0 {
			res.More = true
			break
		}
		size += len(line)
		batch = append(batch, json.RawMessage(bytes.TrimSuffix(line, []byte("\n"))))
		cur.Advance(r.Ref())
	}
	if len(recs) == MaxBatch {
		res.More = true
	}
	if len(batch) > 0 {
		env, err := n.sign(PeerCall{Op: "push", To: p.Peer, Records: batch})
		if err != nil {
			return res, err
		}
		rep, err := p.Link.Call(ctx, env)
		if err == nil && rep.Error != "" {
			err = fmt.Errorf("%s refused the push: %s", p.Peer, rep.Error)
		}
		if err != nil {
			pg.failed(err)
			return res, err
		}
		res.Sent, res.Accepted, res.Duplicates, res.Refused = len(batch), rep.Accepted, rep.Duplicates, rep.Refused
		if rep.Refused > 0 && p.Logf != nil {
			p.Logf("%s refused %d of %d pushed: %s", p.Peer, rep.Refused, len(batch), strings.Join(rep.Problems, "; "))
		}
	}
	err = pg.update(func(s *PeerState) {
		s.Pushed = cur.String()
		if len(batch) > 0 {
			s.PushedAt, s.Sent, s.Error = time.Now().UTC(), s.Sent+res.Accepted, ""
		}
	})
	return res, err
}

// PullOnce asks the peer for what it has past the pulled cursor, waiting
// up to wait for something new, and keeps what the peer may publish here.
func (n *Node) PullOnce(ctx context.Context, p Peering, wait time.Duration) (Result, error) {
	pg, err := n.peering(p)
	if err != nil {
		return Result{}, err
	}
	env, err := n.sign(PeerCall{Op: "pull", To: p.Peer, Cursor: pg.get().Pulled, Limit: MaxBatch, WaitMS: wait.Milliseconds()})
	if err != nil {
		return Result{}, err
	}
	rep, err := p.Link.Call(ctx, env)
	if err == nil && rep.Error != "" {
		err = fmt.Errorf("%s refused the pull: %s", p.Peer, rep.Error)
	}
	if err == nil {
		_, err = ParseCursor(rep.Cursor)
	}
	if err != nil {
		pg.failed(err)
		return Result{}, err
	}
	res := Result{Received: len(rep.Records), More: rep.More}
	var problems []string
	for _, raw := range rep.Records {
		r, err := ParseLine(raw)
		if err == nil && !n.cfg.Rules.Allowed(p.Peer, r.Event.Kind, Publish) {
			err = fmt.Errorf("%v: %s may not publish %s here", r.Ref(), p.Peer, r.Event.Kind)
		}
		var added bool
		if err == nil {
			added, err = n.log.Append(r)
		}
		switch {
		case err != nil:
			res.Refused++
			if len(problems) < maxProblems {
				problems = append(problems, err.Error())
			}
		case added:
			res.Accepted++
		default:
			res.Duplicates++
		}
	}
	if res.Accepted > 0 {
		n.Wake()
	}
	if len(problems) > 0 && p.Logf != nil {
		p.Logf("refused %d of %d pulled from %s: %s", res.Refused, res.Received, p.Peer, strings.Join(problems, "; "))
	}
	if rep.Gap != "" && p.Logf != nil {
		p.Logf("%s's retention dropped events before this node had them: %s", p.Peer, rep.Gap)
	}
	err = pg.update(func(s *PeerState) {
		s.Pulled, s.PulledAt, s.Error = rep.Cursor, time.Now().UTC(), ""
		s.Received += res.Accepted
		s.Refused += res.Refused
		if rep.Gap != "" {
			s.Gap = rep.Gap
		}
	})
	return res, err
}

// Replicate runs a peering until ctx ends: a push whenever this node's log
// grows (and on the backstop), a pull long-polling the peer. A failure
// waits and tries again, from 1s doubling to 30s.
func (n *Node) Replicate(ctx context.Context, p Peering) {
	if p.Wait <= 0 {
		p.Wait = MaxWait
	}
	logf := p.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if _, err := n.peering(p); err != nil {
		logf("peer %s: %v", p.Peer, err)
		return
	}
	var wg sync.WaitGroup
	loop := func(what string, once func() (Result, error), idle func()) {
		defer wg.Done()
		backoff := time.Second
		for ctx.Err() == nil {
			res, err := once()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				logf("%s %s: %v (again in %v)", what, p.Peer, err, backoff)
				select {
				case <-ctx.Done():
				case <-time.After(backoff):
				}
				backoff = min(2*backoff, 30*time.Second)
				continue
			}
			backoff = time.Second
			if !res.More {
				idle()
			}
		}
	}
	if p.Push {
		wg.Add(1)
		var wake <-chan struct{} = n.waiter()
		go loop("push to", func() (Result, error) {
			wake = n.waiter()
			return n.PushOnce(ctx, p)
		}, func() {
			t := time.NewTimer(Backstop)
			defer t.Stop()
			select {
			case <-wake:
			case <-t.C:
			case <-ctx.Done():
			}
		})
	}
	if p.Pull {
		wg.Add(1)
		go loop("pull from", func() (Result, error) { return n.PullOnce(ctx, p, p.Wait) }, func() {})
	}
	wg.Wait()
}

// ---------------------------------------------------------------- links

// SocketLink reaches a peer's node on its unix socket: the dir transport,
// where the peer's address is its var directory on this machine.
type SocketLink struct{ Path string }

func (l SocketLink) Call(ctx context.Context, env PeerEnvelope) (PeerReply, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", l.Path)
	if err != nil {
		return PeerReply{}, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
	defer stop()
	b, err := json.Marshal(Request{Op: "peer", Peer: &env})
	if err != nil {
		return PeerReply{}, err
	}
	if _, err := c.Write(append(b, '\n')); err != nil {
		return PeerReply{}, err
	}
	return CallReply(c)
}

// CallReply reads a node's one reply to op peer off its socket.
func CallReply(r io.Reader) (PeerReply, error) {
	var rep Reply
	if err := readJSONLine(r, &rep); err != nil {
		return PeerReply{}, err
	}
	switch {
	case rep.PeerReply != nil:
		return *rep.PeerReply, nil
	case rep.Error != "":
		return PeerReply{Error: rep.Error}, nil
	}
	return PeerReply{}, errors.New("the peer's node answered without a peer reply")
}

func readJSONLine(r io.Reader, v any) error {
	line, err := readLine(bufio.NewReaderSize(r, 64<<10), MaxPeerCall)
	if err != nil {
		return fmt.Errorf("no reply from the peer's node: %v", err)
	}
	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("the peer's node said something that isn't a reply: %v", err)
	}
	return nil
}

// readLine reads one line of at most max bytes.
func readLine(rd *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := rd.ReadSlice('\n')
		if len(out)+len(chunk) > max {
			return nil, fmt.Errorf("a line over %d bytes", max)
		}
		out = append(out, chunk...)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

// HTTPPath is where a machine's `office serve` takes peer calls for its
// node.
const HTTPPath = "/bus/peer"

// HTTPLink reaches a peer's node through its `office serve` endpoint: the
// http transport, where the peer's address is its URL.
type HTTPLink struct {
	URL    string // the peer's address
	Client *http.Client
}

func (l HTTPLink) Call(ctx context.Context, env PeerEnvelope) (PeerReply, error) {
	u, err := offices.ParseAddress(l.URL)
	if err != nil {
		return PeerReply{}, err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return PeerReply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String()+HTTPPath, bytes.NewReader(b))
	if err != nil {
		return PeerReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	cl := l.Client
	if cl == nil {
		cl = &http.Client{Timeout: MaxWait + 15*time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return PeerReply{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return PeerReply{}, fmt.Errorf("%s: %s %s", u.String()+HTTPPath, resp.Status, strings.TrimSpace(string(msg)))
	}
	var rep PeerReply
	err = readJSONLine(resp.Body, &rep)
	return rep, err
}
