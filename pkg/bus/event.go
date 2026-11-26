// Package bus is the office bus's core: the event a node carries, its
// producer's signature and its region's seal, the vector cursor a reader
// keeps, and the per-producer append-only log a node stores events in.
//
// Every producer (a region's seat, written `<region>/<seat>`, or a service
// with a bare name) numbers its own events with seq, strictly increasing,
// gaps allowed. There is no order across producers; an event that follows
// from another names it in cause. A reader's position is a Cursor, one seq
// per producer.
//
// A Record is an event with two SSH signatures over its canonical bytes:
// the producer's (namespace NSEvent) and the sealing region's
// (NSRegionSeal), so a record can be stored, passed on and checked again
// anywhere, by hand with `ssh-keygen -Y verify` if need be.
package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/Techneosis/the-bowery/internal/offices"
)

// The namespaces: a producer signs an event, its region seals it. Neither
// can pass for the other, or for a placement or a catalog.
const (
	NSEvent      = "bus-event"
	NSRegionSeal = "bus-region"
)

// MaxEventBytes bounds an event's canonical bytes.
const MaxEventBytes = 64 << 10

// The key types are the office package's: ssh-ed25519, as authorized_keys
// and `ssh-keygen -Y` write them.
type (
	PublicKey  = offices.PublicKey
	PrivateKey = offices.PrivateKey
)

var (
	ParsePublicKey = offices.ParsePublicKey
	LoadPrivateKey = offices.LoadPrivateKey
	NewPrivateKey  = offices.NewPrivateKey
)

// Ref names one event: its producer and seq.
type Ref struct {
	Producer string `json:"producer"`
	Seq      uint64 `json:"seq"`
}

func (r Ref) String() string { return fmt.Sprintf("%s:%d", r.Producer, r.Seq) }

// Event is what a producer says happened.
type Event struct {
	V        int             // the envelope's version, 1
	Producer string          // "records", "north/desk"
	Seq      uint64          // the producer's own count, from 1
	Region   string          // the region that seals it
	At       time.Time       // when the producer says it happened
	Kind     string          // its topic: "card.answered", "job.started"
	Owners   []string        // its audience
	Subject  json.RawMessage // what it's about (any JSON)
	Actor    json.RawMessage // who did it (any JSON)
	Cause    *Ref            // the event that led to this one
	Data     json.RawMessage // the rest (any JSON)
}

// Ref is the event's own name.
func (e Event) Ref() Ref { return Ref{Producer: e.Producer, Seq: e.Seq} }

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	kindRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}(\.[a-z][a-z0-9_-]{0,31}){1,3}$`)
)

// ValidProducer is a bare name or `<region>/<seat>`.
func ValidProducer(p string) bool {
	a, b, two := strings.Cut(p, "/")
	return nameRe.MatchString(a) && (!two || nameRe.MatchString(b))
}

// ValidName is a region, seat or owner name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ValidKind is a dotted topic of two to four parts, no wildcards.
func ValidKind(k string) bool { return kindRe.MatchString(k) }

// Validate checks the envelope; it says nothing about who signed it.
func (e Event) Validate() error {
	switch {
	case e.V != 1:
		return fmt.Errorf("version %d; this node reads 1", e.V)
	case !ValidProducer(e.Producer):
		return fmt.Errorf("producer %q isn't a name or region/seat", e.Producer)
	case e.Seq == 0:
		return errors.New("seq starts at 1")
	case !ValidName(e.Region):
		return fmt.Errorf("region %q isn't a name", e.Region)
	case e.At.IsZero():
		return errors.New("no time")
	case !ValidKind(e.Kind):
		return fmt.Errorf("kind %q isn't a topic like job.started", e.Kind)
	case len(e.Owners) > 64:
		return errors.New("more than 64 owners")
	}
	for _, o := range e.Owners {
		if !ValidName(o) {
			return fmt.Errorf("owner %q isn't a name", o)
		}
	}
	if e.Cause != nil && (!ValidProducer(e.Cause.Producer) || e.Cause.Seq == 0) {
		return fmt.Errorf("cause %v doesn't name an event", *e.Cause)
	}
	b, err := e.Canonical()
	if err != nil {
		return err
	}
	if len(b) > MaxEventBytes {
		return fmt.Errorf("the event is %d bytes; at most %d", len(b), MaxEventBytes)
	}
	return nil
}

// wire is an event's canonical shape: field order fixed, empty parts left
// out, times in UTC.
type wire struct {
	V        int             `json:"v"`
	Producer string          `json:"producer"`
	Seq      uint64          `json:"seq"`
	Region   string          `json:"region"`
	At       string          `json:"at"`
	Kind     string          `json:"kind"`
	Owners   []string        `json:"owners,omitempty"`
	Subject  json.RawMessage `json:"subject,omitempty"`
	Actor    json.RawMessage `json:"actor,omitempty"`
	Cause    *Ref            `json:"cause,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
}

// Canonical is the bytes the signatures cover: the event as compact JSON,
// fields in a fixed order, the free-form parts with their keys sorted, no
// HTML escaping, the time as RFC 3339 in UTC. Two spellings of one event
// have the same canonical bytes.
func (e Event) Canonical() ([]byte, error) {
	w := wire{
		V: e.V, Producer: e.Producer, Seq: e.Seq, Region: e.Region,
		At: e.At.UTC().Format(time.RFC3339Nano), Kind: e.Kind,
		Owners: e.Owners, Cause: e.Cause,
	}
	var err error
	for _, p := range []struct {
		name string
		in   json.RawMessage
		out  *json.RawMessage
	}{{"subject", e.Subject, &w.Subject}, {"actor", e.Actor, &w.Actor}, {"data", e.Data, &w.Data}} {
		if *p.out, err = canonicalJSON(p.in); err != nil {
			return nil, fmt.Errorf("%s: %v", p.name, err)
		}
	}
	return marshal(w)
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// canonicalJSON re-encodes one JSON value with sorted keys. Numbers keep
// their text. Empty input (or null) is left out.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("not JSON: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("more than one JSON value")
	}
	if v == nil {
		return nil, nil
	}
	return marshal(v)
}

// parseEvent reads an event's canonical shape strictly.
func parseEvent(raw json.RawMessage) (Event, error) {
	var w wire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Event{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, w.At)
	if err != nil {
		return Event{}, fmt.Errorf("at: %v", err)
	}
	return Event{
		V: w.V, Producer: w.Producer, Seq: w.Seq, Region: w.Region, At: at,
		Kind: w.Kind, Owners: w.Owners, Subject: w.Subject, Actor: w.Actor,
		Cause: w.Cause, Data: w.Data,
	}, nil
}

// Record is a signed and sealed event, as a node stores and sends it.
type Record struct {
	Event Event
	Sig   string // the producer's armored SSH signature
	Seal  string // the region's
}

// Ref is the record's event's name.
func (r Record) Ref() Ref { return r.Event.Ref() }

// Sign signs e as its producer and seals it as its region.
func Sign(e Event, producer, region PrivateKey) (Record, error) {
	if err := e.Validate(); err != nil {
		return Record{}, err
	}
	msg, err := e.Canonical()
	if err != nil {
		return Record{}, err
	}
	sig, err := offices.SignWith(producer, NSEvent, msg)
	if err != nil {
		return Record{}, err
	}
	seal, err := offices.SignWith(region, NSRegionSeal, msg)
	if err != nil {
		return Record{}, err
	}
	return Record{Event: e, Sig: string(sig), Seal: string(seal)}, nil
}

// Keys are the keys a node has pinned: which may sign as a producer, and
// which may seal as a region.
type Keys interface {
	ProducerKeys(producer string) []PublicKey
	RegionKeys(region string) []PublicKey
}

// Verify checks the envelope, that a region's seat is sealed by its own
// region, the producer's signature by one of its pinned keys and the seal
// by one of its region's.
func (r Record) Verify(k Keys) error {
	e := r.Event
	if err := e.Validate(); err != nil {
		return err
	}
	if reg, _, seat := strings.Cut(e.Producer, "/"); seat && reg != e.Region {
		return fmt.Errorf("%s is region %s's, sealed as %s", e.Producer, reg, e.Region)
	}
	msg, err := e.Canonical()
	if err != nil {
		return err
	}
	if err := signedBy(r.Sig, NSEvent, msg, k.ProducerKeys(e.Producer)); err != nil {
		return fmt.Errorf("%v: producer %s: %v", e.Ref(), e.Producer, err)
	}
	if err := signedBy(r.Seal, NSRegionSeal, msg, k.RegionKeys(e.Region)); err != nil {
		return fmt.Errorf("%v: region %s's seal: %v", e.Ref(), e.Region, err)
	}
	return nil
}

func signedBy(armored, ns string, msg []byte, pinned []PublicKey) error {
	if len(pinned) == 0 {
		return errors.New("no key pinned")
	}
	got, err := offices.VerifySignature([]byte(armored), ns, msg)
	if err != nil {
		return err
	}
	for _, p := range pinned {
		if got.Equal(p) {
			return nil
		}
	}
	return fmt.Errorf("signed by %s, which isn't pinned", got.Fingerprint())
}

type recordWire struct {
	Event json.RawMessage `json:"event"`
	Sig   string          `json:"sig"`
	Seal  string          `json:"seal"`
}

// Line is the record as one line of JSON, newline included.
func (r Record) Line() ([]byte, error) {
	ev, err := r.Event.Canonical()
	if err != nil {
		return nil, err
	}
	b, err := marshal(recordWire{Event: ev, Sig: r.Sig, Seal: r.Seal})
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ParseLine reads one record's line, strictly: one JSON object, the known
// fields only. It doesn't verify.
func ParseLine(line []byte) (Record, error) {
	var w recordWire
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Record{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return Record{}, errors.New("more than one record on a line")
	}
	if len(w.Event) == 0 || string(w.Event) == "null" {
		return Record{}, errors.New("no event")
	}
	e, err := parseEvent(w.Event)
	if err != nil {
		return Record{}, fmt.Errorf("event: %v", err)
	}
	return Record{Event: e, Sig: w.Sig, Seal: w.Seal}, nil
}
