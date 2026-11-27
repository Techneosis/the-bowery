package bus

// The private projection: a region's private records (private.card,
// private.request, private.ticket, private.hide and private.note, which
// never leave the region's node) folded into cards, requests and tickets,
// in the keyed snapshot shape a board's client already draws:
//
//	{"v":1,"cursor":"north/alice=12","meta":{"skipped":[]},
//	 "lists":{"card":[{"key":"private/watching","v":{…}}],"request":[…],
//	          "ticket":[…],"hide":[…],"note":[…]}}
//
// Every list is always there. An entry's key is its record's key with the
// "private/" prefix (a record that gives none is keyed by its producer and
// seq), so a private key can't be mistaken for a shared one, and a shared
// record that names one is refused at the node (NamesPrivate).
//
// Private may point at shared, never the reverse: a private record's refs,
// members, under, on and hide name any key, shared or private; a shared
// record is never folded here, whatever it says.
//
// Records for one key apply in (at, producer, seq) order, each merging its
// fields over the ones before; remove (undo, for a hide) starts the key
// afresh. So the fold is the same whatever order a reader got the records
// in and however often it saw one: fold parity, replay twice and the bytes
// are the same (projection_test.go).

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// PrivatePrefix starts every private record's key.
const PrivatePrefix = "private/"

// PrivateLists is every list a private snapshot has, in order.
var PrivateLists = []string{"card", "request", "ticket", "hide", "note"}

var privateKinds = map[string]string{
	"private.card": "card", "private.request": "request", "private.ticket": "ticket",
	"private.hide": "hide", "private.note": "note",
}

// PrivateKinds are the topics the projection folds.
var PrivateKinds = []string{"private.card", "private.request", "private.ticket", "private.hide", "private.note"}

const (
	maxPrivRefs  = 64
	maxPrivShort = 300
	maxPrivLong  = 16000
)

var privKeyRe = regexp.MustCompile(`^(private/)?[a-z0-9][a-z0-9._-]{0,79}$`)

// PrivateLink is a card's link: where it goes.
type PrivateLink struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// privIn is a private record's data: every topic's fields (CheckPrivate
// says which each may have).
type privIn struct {
	Key     string        `json:"key,omitempty"`
	Remove  bool          `json:"remove,omitempty"`
	Undo    bool          `json:"undo,omitempty"`
	Subject string        `json:"subject,omitempty"`
	Summary string        `json:"summary,omitempty"`
	Body    string        `json:"body,omitempty"`
	Area    string        `json:"area,omitempty"`
	Urgency string        `json:"urgency,omitempty"`
	Kind    string        `json:"kind,omitempty"`
	State   string        `json:"state,omitempty"`
	Steps   []string      `json:"steps,omitempty"`
	Links   []PrivateLink `json:"links,omitempty"`
	Refs    []string      `json:"refs,omitempty"`
	Title   string        `json:"title,omitempty"`
	Under   string        `json:"under,omitempty"`
	Members []string      `json:"members,omitempty"`
	On      string        `json:"on,omitempty"`
	Text    string        `json:"text,omitempty"`
}

// The fields each topic takes.
var privFields = map[string][]string{
	"card":    {"key", "remove", "subject", "summary", "body", "area", "urgency", "kind", "state", "steps", "links", "refs"},
	"request": {"key", "remove", "title", "state", "area", "under", "members"},
	"ticket":  {"key", "remove", "title", "state", "area", "body", "refs"},
	"hide":    {"key", "undo"},
	"note":    {"key", "remove", "on", "text"},
}

// CheckPrivate says whether data is a record the projection folds under
// kind: one of PrivateKinds, its data a JSON object with that topic's
// fields only, each within bounds. A publisher checks before it publishes
// (office bus publish does); the fold skips, and names, what doesn't.
func CheckPrivate(kind string, data json.RawMessage) error {
	_, err := checkPrivate(kind, data)
	return err
}

func checkPrivate(kind string, data json.RawMessage) (privIn, error) {
	var in privIn
	if !strings.HasPrefix(kind, "private.") {
		return in, fmt.Errorf("%s isn't a private topic (private.*)", kind)
	}
	list, ok := privateKinds[kind]
	if !ok {
		return in, fmt.Errorf("%s isn't a topic the private projection folds: %s", kind, strings.Join(PrivateKinds, ", "))
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return in, fmt.Errorf("%s needs its data, a JSON object", kind)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return in, fmt.Errorf("%s: its data isn't a JSON object", kind)
	}
	for f := range fields {
		if !contains(privFields[list], f) {
			return in, fmt.Errorf("%s has no field %q (it takes %s)", kind, f, strings.Join(privFields[list], ", "))
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%s: %v", kind, err)
	}
	bad := func(f string, a ...any) (privIn, error) { return in, fmt.Errorf(kind+": "+f, a...) }
	if list == "hide" {
		if !refOK(in.Key) {
			return bad("key names what to hide: a key, no spaces, at most 200 bytes")
		}
		return in, nil
	}
	if in.Key != "" && !privKeyRe.MatchString(in.Key) {
		return bad("key %q: lowercase letters, digits, . _ - (at most 80), private/ optional", in.Key)
	}
	if in.Remove {
		if in.Key == "" {
			return bad("remove needs the key it removes")
		}
		return in, nil
	}
	for _, s := range []struct{ name, v string }{{"subject", in.Subject}, {"summary", in.Summary}, {"area", in.Area},
		{"urgency", in.Urgency}, {"title", in.Title}} {
		if len(s.v) > maxPrivShort {
			return bad("%s is over %d bytes", s.name, maxPrivShort)
		}
	}
	if len(in.Body) > maxPrivLong || len(in.Text) > maxPrivLong {
		return bad("body or text is over %d bytes", maxPrivLong)
	}
	if in.State != "" && in.State != "open" && in.State != "done" {
		return bad("state %q: open or done", in.State)
	}
	for _, refs := range [][]string{in.Refs, in.Members} {
		if len(refs) > maxPrivRefs {
			return bad("more than %d keys named", maxPrivRefs)
		}
		for _, r := range refs {
			if !refOK(r) {
				return bad("%q isn't a key (no spaces, at most 200 bytes)", r)
			}
		}
	}
	if in.Under != "" && !refOK(in.Under) {
		return bad("under %q isn't a key", in.Under)
	}
	switch list {
	case "card":
		if in.Key == "" && in.Subject == "" {
			return bad("a new card needs a subject")
		}
		switch in.Kind {
		case "", "fyi", "decision", "hands", "review":
		default:
			return bad("kind %q: fyi, decision, hands or review", in.Kind)
		}
		if len(in.Steps) > maxPrivRefs || len(in.Links) > maxPrivRefs {
			return bad("more than %d steps or links", maxPrivRefs)
		}
		for _, s := range in.Steps {
			if len(s) > maxPrivShort*4 {
				return bad("a step is over %d bytes", maxPrivShort*4)
			}
		}
		for _, l := range in.Links {
			if !strings.HasPrefix(l.URL, "https://") && !strings.HasPrefix(l.URL, "http://") {
				return bad("a link's url must be http(s): %q", l.URL)
			}
			if len(l.URL) > 2000 || len(l.Title) > maxPrivShort {
				return bad("a link is too long")
			}
		}
	case "request", "ticket":
		if in.Key == "" && in.Title == "" {
			return bad("a new %s needs a title", list)
		}
	case "note":
		if in.Key == "" && (in.On == "" || in.Text == "") {
			return bad("a new note needs on (the key it's about) and text")
		}
		if in.On != "" && !refOK(in.On) {
			return bad("on %q isn't a key", in.On)
		}
	}
	return in, nil
}

func refOK(s string) bool {
	return s != "" && len(s) <= 200 && !strings.ContainsAny(s, " \t\r\n")
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// privKey is the entry key a record's key gives: with the prefix, or its
// ref's when it gives none.
func privKey(key string, r Ref) string {
	if key == "" {
		return PrivatePrefix + strings.ReplaceAll(r.Producer, "/", "-") + "-" + fmt.Sprint(r.Seq)
	}
	return PrivatePrefix + strings.TrimPrefix(key, PrivatePrefix)
}

// NamesPrivate says whether a JSON value holds a private key anywhere: a
// string, or an object's key, starting "private/". A node refuses a shared
// topic's event that does: shared never points at private.
func NamesPrivate(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if s, ok := tok.(string); ok && strings.HasPrefix(s, PrivatePrefix) {
			return true
		}
	}
}

// PrivateEntry is one list's entry: its key, and its projection.
type PrivateEntry struct {
	Key string          `json:"key"`
	V   json.RawMessage `json:"v"`
}

// PrivateState is the snapshot: the cursor past every private record
// folded, meta (skipped: the private records it couldn't fold, and why),
// and the lists.
type PrivateState struct {
	V      int                        `json:"v"`
	Cursor string                     `json:"cursor"`
	Meta   map[string]json.RawMessage `json:"meta"`
	Lists  map[string][]PrivateEntry  `json:"lists"`
}

// Bytes is the snapshot as served: compact, map keys sorted, no HTML
// escaping. Two folds of the same records have the same bytes.
func (s PrivateState) Bytes() ([]byte, error) { return marshal(s) }

// Find is the entry keyed key, and its list's name.
func (s PrivateState) Find(key string) (string, PrivateEntry, bool) {
	for _, l := range PrivateLists {
		for _, e := range s.Lists[l] {
			if e.Key == key {
				return l, e, true
			}
		}
	}
	return "", PrivateEntry{}, false
}

// PrivateSkip is a private record the fold couldn't use.
type PrivateSkip struct {
	Ref   string `json:"ref"`
	Kind  string `json:"kind"`
	Error string `json:"error"`
}

type privEv struct {
	ref    Ref
	at     time.Time
	fields map[string]json.RawMessage
	reset  bool
}

func (a privEv) before(b privEv) bool {
	if !a.at.Equal(b.at) {
		return a.at.Before(b.at)
	}
	if a.ref.Producer != b.ref.Producer {
		return a.ref.Producer < b.ref.Producer
	}
	return a.ref.Seq < b.ref.Seq
}

// PrivateFold folds private records as they come; State is the snapshot
// at any point. It reads records a node has already verified, and isn't
// safe for concurrent use.
type PrivateFold struct {
	cursor  Cursor
	seen    map[Ref]bool
	keys    map[string]map[string][]privEv // list, key: its records
	skipped []PrivateSkip
}

// NewPrivateFold is an empty fold.
func NewPrivateFold() *PrivateFold {
	f := &PrivateFold{cursor: Cursor{}, seen: map[Ref]bool{}, keys: map[string]map[string][]privEv{}}
	for _, l := range PrivateLists {
		f.keys[l] = map[string][]privEv{}
	}
	return f
}

// Apply folds one record in. A shared record, or one already folded,
// changes nothing.
func (f *PrivateFold) Apply(r Record) {
	e := r.Event
	if !strings.HasPrefix(e.Kind, "private.") || f.seen[e.Ref()] {
		return
	}
	f.seen[e.Ref()] = true
	f.cursor.Advance(e.Ref())
	in, err := checkPrivate(e.Kind, e.Data)
	if err != nil {
		f.skipped = append(f.skipped, PrivateSkip{Ref: e.Ref().String(), Kind: e.Kind, Error: err.Error()})
		return
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(e.Data, &fields)
	list := privateKinds[e.Kind]
	key := in.Key
	if list != "hide" {
		key = privKey(in.Key, e.Ref())
	}
	delete(fields, "key")
	delete(fields, "remove")
	delete(fields, "undo")
	f.keys[list][key] = append(f.keys[list][key], privEv{ref: e.Ref(), at: e.At.UTC(), fields: fields, reset: in.Remove || in.Undo})
}

type privMerged struct {
	in            privIn
	from, by, ref string
	sent, updated time.Time
}

func (f *PrivateFold) merged(list string) map[string]privMerged {
	out := map[string]privMerged{}
	for key, evs := range f.keys[list] {
		evs = append([]privEv(nil), evs...)
		sort.Slice(evs, func(i, j int) bool { return evs[i].before(evs[j]) })
		var cur map[string]json.RawMessage
		var m privMerged
		for _, ev := range evs {
			if ev.reset {
				cur = nil
				continue
			}
			if cur == nil {
				cur = map[string]json.RawMessage{}
				m = privMerged{from: ev.ref.Producer, sent: ev.at}
			}
			for k, v := range ev.fields {
				cur[k] = v
			}
			m.by, m.ref, m.updated = ev.ref.Producer, ev.ref.String(), ev.at
		}
		if cur == nil {
			continue
		}
		b, _ := json.Marshal(cur)
		_ = json.Unmarshal(b, &m.in)
		out[key] = m
	}
	return out
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func orNone[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func orOpen(s string) string {
	if s == "" {
		return "open"
	}
	return s
}

type privCard struct {
	ID      string        `json:"id"`
	From    string        `json:"from"`
	By      string        `json:"by"`
	Subject string        `json:"subject"`
	Summary string        `json:"summary"`
	Body    string        `json:"body"`
	Area    string        `json:"area"`
	Urgency string        `json:"urgency"`
	Kind    string        `json:"kind"`
	State   string        `json:"state"`
	Steps   []string      `json:"steps"`
	Links   []PrivateLink `json:"links"`
	Refs    []string      `json:"refs"`
	Sent    string        `json:"sent"`
	Updated string        `json:"updated"`
	Ref     string        `json:"ref"`
	Private bool          `json:"private"`
}

type privRequest struct {
	K       string   `json:"k"`
	T       string   `json:"t"`
	O       bool     `json:"o"`
	State   string   `json:"state"`
	Area    string   `json:"area,omitempty"`
	Under   string   `json:"under,omitempty"`
	Members []string `json:"members"`
	Nested  int      `json:"nested"`
	Cards   int      `json:"cards"`
	Wait    int      `json:"wait"`
	From    string   `json:"from"`
	By      string   `json:"by"`
	Sent    string   `json:"sent"`
	Updated string   `json:"updated"`
	Ref     string   `json:"ref"`
	Private bool     `json:"private"`
}

type privTicket struct {
	K       string   `json:"k"`
	T       string   `json:"t"`
	State   string   `json:"state"`
	Area    string   `json:"area"`
	Body    string   `json:"body"`
	Refs    []string `json:"refs"`
	From    string   `json:"from"`
	By      string   `json:"by"`
	Sent    string   `json:"sent"`
	Updated string   `json:"updated"`
	Ref     string   `json:"ref"`
	Private bool     `json:"private"`
}

type privHide struct {
	Key     string `json:"key"`
	From    string `json:"from"`
	At      string `json:"at"`
	Ref     string `json:"ref"`
	Private bool   `json:"private"`
}

type privNote struct {
	ID      string `json:"id"`
	On      string `json:"on"`
	Text    string `json:"text"`
	From    string `json:"from"`
	By      string `json:"by"`
	Sent    string `json:"sent"`
	Updated string `json:"updated"`
	Ref     string `json:"ref"`
	Private bool   `json:"private"`
}

// State is the snapshot of everything folded so far; folding goes on
// after it unchanged.
func (f *PrivateFold) State() PrivateState {
	s := PrivateState{V: 1, Cursor: f.cursor.String(), Meta: map[string]json.RawMessage{}, Lists: map[string][]PrivateEntry{}}
	skipped := append([]PrivateSkip{}, f.skipped...)
	sort.Slice(skipped, func(i, j int) bool {
		a, b := refOf(skipped[i].Ref), refOf(skipped[j].Ref)
		if a.Producer != b.Producer {
			return a.Producer < b.Producer
		}
		return a.Seq < b.Seq
	})
	s.Meta["skipped"] = mustRaw(skipped)

	cards, reqs := f.merged("card"), f.merged("request")
	cardsNaming := map[string]int{}
	for _, c := range cards {
		for _, r := range dedupe(c.in.Refs) {
			cardsNaming[r]++
		}
	}
	nested := map[string]int{}
	for _, r := range reqs {
		if r.in.Under != "" {
			nested[r.in.Under]++
		}
	}
	type built struct {
		key     string
		updated time.Time
		v       any
	}
	lists := map[string][]built{}
	for k, m := range cards {
		lists["card"] = append(lists["card"], built{k, m.updated, privCard{
			ID: k, From: m.from, By: m.by, Subject: m.in.Subject, Summary: m.in.Summary, Body: m.in.Body,
			Area: m.in.Area, Urgency: m.in.Urgency, Kind: m.in.Kind, State: orOpen(m.in.State),
			Steps: orNone(m.in.Steps), Links: orNone(m.in.Links), Refs: orNone(m.in.Refs),
			Sent: ts(m.sent), Updated: ts(m.updated), Ref: m.ref, Private: true}})
	}
	for k, m := range reqs {
		st := orOpen(m.in.State)
		lists["request"] = append(lists["request"], built{k, m.updated, privRequest{
			K: k, T: m.in.Title, O: st == "open", State: st, Area: m.in.Area, Under: m.in.Under,
			Members: orNone(m.in.Members), Nested: nested[k], Cards: cardsNaming[k],
			From: m.from, By: m.by, Sent: ts(m.sent), Updated: ts(m.updated), Ref: m.ref, Private: true}})
	}
	for k, m := range f.merged("ticket") {
		lists["ticket"] = append(lists["ticket"], built{k, m.updated, privTicket{
			K: k, T: m.in.Title, State: orOpen(m.in.State), Area: m.in.Area, Body: m.in.Body, Refs: orNone(m.in.Refs),
			From: m.from, By: m.by, Sent: ts(m.sent), Updated: ts(m.updated), Ref: m.ref, Private: true}})
	}
	for k, m := range f.merged("hide") {
		lists["hide"] = append(lists["hide"], built{k, m.updated, privHide{
			Key: k, From: m.by, At: ts(m.updated), Ref: m.ref, Private: true}})
	}
	for k, m := range f.merged("note") {
		lists["note"] = append(lists["note"], built{k, m.updated, privNote{
			ID: k, On: m.in.On, Text: m.in.Text, From: m.from, By: m.by,
			Sent: ts(m.sent), Updated: ts(m.updated), Ref: m.ref, Private: true}})
	}
	// Newest first, then by key: one order for one set of records.
	for _, l := range PrivateLists {
		bs := lists[l]
		sort.Slice(bs, func(i, j int) bool {
			if !bs[i].updated.Equal(bs[j].updated) {
				return bs[i].updated.After(bs[j].updated)
			}
			return bs[i].key < bs[j].key
		})
		out := []PrivateEntry{}
		for _, b := range bs {
			out = append(out, PrivateEntry{Key: b.key, V: mustRaw(b.v)})
		}
		s.Lists[l] = out
	}
	return s
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func refOf(s string) Ref {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return Ref{Producer: s}
	}
	var seq uint64
	fmt.Sscan(s[i+1:], &seq)
	return Ref{Producer: s[:i], Seq: seq}
}

func mustRaw(v any) json.RawMessage {
	b, err := marshal(v)
	if err != nil {
		// Every value here is the fold's own: one that won't marshal is a
		// bug, shown as such rather than dropped.
		b, _ = marshal(map[string]string{"err": err.Error()})
	}
	return b
}

// errNamesPrivate is a shared event that names a private key.
var errNamesPrivate = errors.New("a shared topic's event may not name a private record (a private/ key): private points at shared, never the reverse")
