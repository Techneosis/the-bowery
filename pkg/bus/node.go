package bus

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Techneosis/the-bowery/internal/offices"
)

// DefaultBuffer is how many records a node holds for one follower before it
// gives up on it: past that the follower is told where to resume from and
// the connection is closed, so a slow reader never holds the node's memory
// or anyone else's events.
const DefaultBuffer = 1024

// DefaultStall is how long a follower's buffer may stay full.
const DefaultStall = 5 * time.Second

// Backstop is how often a follower looks at the log with no wake at all: a
// writer the node didn't hear from (another handle, a platform with no
// file watch) is seen within it.
const Backstop = time.Second

// NodeConfig is what a local node needs: its log, its region's rules, and
// the keys it signs and seals its own seats' events with.
type NodeConfig struct {
	Dir    string // the log's directory
	Region string
	Rules  Rules
	// Sign is the key the node signs its seats' events with: it speaks for
	// the seat whose Unix user it saw at the socket. Seal is the region's.
	Sign, Seal PrivateKey
	// Keys are any further keys pinned (peers' producers and regions);
	// this region's own seats and seal are pinned from Sign and Seal.
	Keys Keys
	// Peers are the peer regions this node replicates with, each by its
	// pinned region key: it authenticates their calls, seals their events
	// and signs for their seats.
	Peers PeerKeys
	// Buffer is a follower's buffer (default DefaultBuffer), and Stall how
	// long it may stay full (default DefaultStall) before the follower is
	// told where to resume from and let go. A reader that keeps reading
	// is never let go, however big its backlog.
	Buffer int
	Stall  time.Duration
	// Dev lets a request name its seat (Request.As): dev mode, where the
	// socket's peer is always the invoking user.
	Dev bool
	Now func() time.Time
	// Logf is where the node says what it refused on arrival from a peer
	// (a private topic above all), beside the reply the sender gets. nil:
	// nowhere.
	Logf func(format string, a ...any)
}

// Node is a region's local bus node: its own seats publish through it (it
// signs as the seat and seals as the region) and follow it with a vector
// cursor, each limited to the topics the region's rules give that seat.
type Node struct {
	cfg  NodeConfig
	log  *Log
	lock *os.File

	pubMu     sync.Mutex
	wakeMu    sync.Mutex
	wake      chan struct{}
	followers atomic.Int64

	seenMu  sync.Mutex
	seen    map[string]time.Time // peer calls answered, until they'd be stale
	peersMu sync.Mutex
	peers   map[string]*peering
}

type nodeKeys struct {
	region     string
	sign, seal PublicKey
	more       Keys
	peers      PeerKeys
}

func (k nodeKeys) ProducerKeys(p string) []PublicKey {
	var out []PublicKey
	if strings.HasPrefix(p, k.region+"/") {
		out = append(out, k.sign)
	}
	if k.more != nil {
		out = append(out, k.more.ProducerKeys(p)...)
	}
	return append(out, k.peers.ProducerKeys(p)...)
}

func (k nodeKeys) RegionKeys(r string) []PublicKey {
	var out []PublicKey
	if r == k.region {
		out = append(out, k.seal)
	}
	if k.more != nil {
		out = append(out, k.more.RegionKeys(r)...)
	}
	return append(out, k.peers.RegionKeys(r)...)
}

// NewNode opens the node's log. One node serves a directory: a second one
// is refused while the first holds its lock.
func NewNode(cfg NodeConfig) (*Node, error) {
	if !ValidName(cfg.Region) {
		return nil, fmt.Errorf("region %q isn't a name", cfg.Region)
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = DefaultBuffer
	}
	if cfg.Stall <= 0 {
		cfg.Stall = DefaultStall
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if _, ok := cfg.Peers[cfg.Region]; ok {
		return nil, fmt.Errorf("region %s can't be its own peer", cfg.Region)
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	lp := filepath.Join(cfg.Dir, "node.lock")
	lock, err := os.OpenFile(lp, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another node is serving %s", cfg.Dir)
	}
	log, err := OpenLog(cfg.Dir, nodeKeys{region: cfg.Region, sign: cfg.Sign.Public, seal: cfg.Seal.Public, more: cfg.Keys, peers: cfg.Peers})
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &Node{cfg: cfg, log: log, lock: lock, wake: make(chan struct{})}, nil
}

// Close closes the log and lets another node take the directory.
func (n *Node) Close() error {
	err := n.log.Close()
	n.lock.Close()
	return err
}

// Log is the node's store.
func (n *Node) Log() *Log { return n.log }

// Wake tells every follower to look at the log now. Publish does it; so
// does a watch on the log's directory, for writers the node didn't see.
func (n *Node) Wake() {
	n.wakeMu.Lock()
	close(n.wake)
	n.wake = make(chan struct{})
	n.wakeMu.Unlock()
}

func (n *Node) waiter() <-chan struct{} {
	n.wakeMu.Lock()
	defer n.wakeMu.Unlock()
	return n.wake
}

// Compact drops what the region's retention no longer keeps.
func (n *Node) Compact() (int, error) {
	return n.log.Compact(n.cfg.Now(), n.cfg.Rules.Retention)
}

// Publication is what a seat says; the node fills in the rest.
type Publication struct {
	Kind    string          `json:"kind"`
	Owners  []string        `json:"owners,omitempty"`
	Subject json.RawMessage `json:"subject,omitempty"`
	Actor   json.RawMessage `json:"actor,omitempty"`
	Cause   *Ref            `json:"cause,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Producer is a local seat's producer name.
func (n *Node) Producer(seat string) string { return n.cfg.Region + "/" + seat }

// Publish adds an event as a local seat: the seat's next seq, the node's
// clock, signed and sealed, if the rules let the seat publish its topic.
func (n *Node) Publish(seat string, p Publication) (Record, error) {
	if !ValidName(seat) {
		return Record{}, fmt.Errorf("%q isn't a seat name", seat)
	}
	if !ValidKind(p.Kind) {
		return Record{}, fmt.Errorf("kind %q isn't a topic like job.started", p.Kind)
	}
	if !n.cfg.Rules.SeatAllowed(seat, p.Kind, Publish) {
		return Record{}, fmt.Errorf("seat %s may not publish %s (the region file's bus.seats.%s.publish)", seat, p.Kind, seat)
	}
	n.pubMu.Lock()
	producer := n.Producer(seat)
	e := Event{
		V: 1, Producer: producer, Seq: n.log.Heads()[producer] + 1, Region: n.cfg.Region,
		At: n.cfg.Now().UTC(), Kind: p.Kind, Owners: p.Owners, Subject: p.Subject, Actor: p.Actor,
		Cause: p.Cause, Data: p.Data,
	}
	r, err := Sign(e, n.cfg.Sign, n.cfg.Seal)
	if err == nil {
		var added bool
		if added, err = n.log.Append(r); err == nil && !added {
			err = fmt.Errorf("%v is in the log already", r.Ref())
		}
	}
	n.pubMu.Unlock()
	if err != nil {
		return Record{}, err
	}
	n.Wake()
	return r, nil
}

// Request is one connection's one request: a line of JSON.
type Request struct {
	Op string `json:"op"` // publish, follow, status; peer for a peer region's call
	// As names the seat in dev mode; otherwise the socket's peer is the seat.
	As          string       `json:"as,omitempty"`
	Publication *Publication `json:"publication,omitempty"`
	// Follow: from Cursor (empty is the start), only these topics (within
	// the seat's own), and with Once, stop when caught up.
	Cursor string   `json:"cursor,omitempty"`
	Topics []string `json:"topics,omitempty"`
	Once   bool     `json:"once,omitempty"`
	// Peer is a peer region's signed call (op peer): the signature, not
	// the socket's user, says who is calling.
	Peer *PeerEnvelope `json:"peer,omitempty"`
}

// Status is what a seat sees of the node.
type Status struct {
	Region     string   `json:"region"`
	Seat       string   `json:"seat"`
	Producer   string   `json:"producer"`
	Heads      string   `json:"heads"`
	Floors     string   `json:"floors"`
	MayPublish []string `json:"may_publish"`
	MayRead    []string `json:"may_read"`
	Followers  int64    `json:"followers"`
	// Peers are the peerings' cursors and last word.
	Peers []PeerState `json:"peers"`
}

// Reply is one line back. A follow sends records, one caught_up once its
// backlog is sent, and ends with an error or, when it fell too far behind,
// End and the Cursor to resume from.
type Reply struct {
	Error    string           `json:"error,omitempty"`
	Floors   string           `json:"floors,omitempty"` // with a too-old error: resync from here
	Ref      *Ref             `json:"ref,omitempty"`
	Record   *json.RawMessage `json:"record,omitempty"`
	CaughtUp bool             `json:"caught_up,omitempty"`
	Cursor   string           `json:"cursor,omitempty"`
	End      string           `json:"end,omitempty"`
	Status   *Status          `json:"status,omitempty"`
	// PeerReply answers op peer.
	PeerReply *PeerReply `json:"peer_reply,omitempty"`
}

const writeTimeout = 30 * time.Second

func writeReply(c net.Conn, r Reply) error {
	b, err := marshal(r)
	if err != nil {
		return err
	}
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err = c.Write(append(b, '\n'))
	return err
}

func recordReply(r Record) (Reply, error) {
	line, err := r.Line()
	if err != nil {
		return Reply{}, err
	}
	raw := json.RawMessage(line[:len(line)-1])
	ref := r.Ref()
	return Reply{Ref: &ref, Record: &raw}, nil
}

// Serve answers one connection whose peer is the Unix user seat, and
// closes it.
func (n *Node) Serve(c net.Conn, seat string) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	rd := bufio.NewReaderSize(c, 64<<10)
	line, err := readLine(rd, MaxPeerCall)
	if err != nil {
		writeReply(c, Reply{Error: "no request: one line of JSON"})
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		writeReply(c, Reply{Error: "the request isn't JSON: " + err.Error()})
		return
	}
	c.SetReadDeadline(time.Time{})
	if req.Op == "peer" {
		if req.Peer == nil {
			writeReply(c, Reply{Error: "peer needs a signed call"})
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			// The caller hanging up ends a long-poll.
			if _, err := rd.ReadByte(); err != nil {
				cancel()
			}
		}()
		rep := n.HandlePeer(ctx, *req.Peer)
		writeReply(c, Reply{PeerReply: &rep})
		return
	}
	if req.As != "" && req.As != seat {
		if !n.cfg.Dev {
			writeReply(c, Reply{Error: "as is for dev mode only: the seat is the socket's Unix user"})
			return
		}
		seat = req.As
	}
	switch req.Op {
	case "publish":
		if req.Publication == nil {
			writeReply(c, Reply{Error: "publish needs a publication"})
			return
		}
		r, err := n.Publish(seat, *req.Publication)
		if err != nil {
			writeReply(c, Reply{Error: err.Error()})
			return
		}
		rep, err := recordReply(r)
		if err != nil {
			rep = Reply{Error: err.Error()}
		}
		writeReply(c, rep)
	case "status":
		t := n.cfg.Rules.Seats[seat]
		writeReply(c, Reply{Status: &Status{
			Region: n.cfg.Region, Seat: seat, Producer: n.Producer(seat),
			Heads: n.log.Heads().String(), Floors: n.log.Floors().String(),
			MayPublish: nonNil(t.Publish), MayRead: nonNil(t.Read), Followers: n.followers.Load(),
			Peers: n.PeerStatus(),
		}})
	case "follow":
		n.follow(c, rd, seat, req)
	default:
		writeReply(c, Reply{Error: fmt.Sprintf("unknown op %q: publish, follow or status", req.Op)})
	}
}

func nonNil(p Patterns) []string {
	if p == nil {
		return []string{}
	}
	return p
}

// item is one reply queued for a follower, with the refs it moves the
// follower's cursor past once written.
type item struct {
	reply Reply
	ref   *Ref
	upTo  Cursor // a caught_up: the follower has seen everything to here
}

func (n *Node) follow(c net.Conn, rd *bufio.Reader, seat string, req Request) {
	cur, err := ParseCursor(req.Cursor)
	if err != nil {
		writeReply(c, Reply{Error: err.Error()})
		return
	}
	for _, t := range req.Topics {
		if !offices.ValidTopicPattern(t) {
			writeReply(c, Reply{Error: fmt.Sprintf(`%q isn't a topic pattern ("*", "job.*" or job.started)`, t)})
			return
		}
	}
	topics := Patterns(req.Topics)
	n.followers.Add(1)
	defer n.followers.Add(-1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The follower says nothing more; its end of the connection closing is
	// the end of the follow.
	go func() {
		for {
			if _, err := rd.ReadByte(); err != nil {
				cancel()
				return
			}
		}
	}()

	out := make(chan item, n.cfg.Buffer)
	stop := make(chan struct{}) // the buffer overflowed
	done := make(chan struct{}) // the writer has finished
	sent := cur.Clone()
	go func() {
		defer close(done)
		defer cancel()
		end := func() {
			s := sent.String()
			writeReply(c, Reply{End: "resume from " + s, Cursor: s})
		}
		for {
			select {
			case <-stop:
				end()
				return
			default:
			}
			select {
			case it, ok := <-out:
				if !ok {
					return
				}
				if err := writeReply(c, it.reply); err != nil {
					return
				}
				if it.ref != nil {
					sent.Advance(*it.ref)
				}
				for p, s := range it.upTo {
					sent.Advance(Ref{p, s})
				}
			case <-stop:
				end()
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// put queues an item; false means stop following.
	overflowed := false
	put := func(it item) bool {
		select {
		case out <- it:
			return true
		case <-done:
			return false
		default:
		}
		t := time.NewTimer(n.cfg.Stall)
		defer t.Stop()
		select {
		case out <- it:
			return true
		case <-done:
			return false
		case <-t.C:
			overflowed = true
			close(stop)
			return false
		}
	}
	defer func() {
		if !overflowed {
			close(out)
		}
		<-done
	}()

	scan := cur
	caughtUp := false
	const batch = 256
	for {
		wake := n.waiter()
		recs, err := n.log.Since(scan, batch)
		if err != nil {
			rep := Reply{Error: err.Error()}
			var too *TooOldError
			if errors.As(err, &too) {
				rep.Floors = too.Floors.String()
			}
			put(item{reply: rep})
			return
		}
		for _, r := range recs {
			scan.Advance(r.Ref())
			k := r.Event.Kind
			if !n.cfg.Rules.SeatAllowed(seat, k, Read) || (len(topics) > 0 && !topics.Match(k)) {
				continue
			}
			rep, err := recordReply(r)
			if err != nil {
				put(item{reply: Reply{Error: err.Error()}})
				return
			}
			if !put(item{reply: rep, ref: rep.Ref}) {
				return
			}
		}
		if len(recs) == batch {
			continue
		}
		if !caughtUp {
			caughtUp = true
			if !put(item{reply: Reply{CaughtUp: true, Cursor: scan.String()}, upTo: scan.Clone()}) {
				return
			}
			if req.Once {
				return
			}
		}
		t := time.NewTimer(Backstop)
		select {
		case <-wake:
		case <-t.C:
		case <-ctx.Done():
		case <-done:
		}
		t.Stop()
		if ctx.Err() != nil {
			return
		}
	}
}
