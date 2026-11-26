package bus

import (
	"time"

	"github.com/Techneosis/the-bowery/internal/offices"
)

// Dir is which way an event goes, seen from this region's node.
type Dir int

const (
	// Publish is into this node: a peer or a local seat sending an event.
	Publish Dir = iota
	// Read is out of this node: a peer or a local seat being sent one.
	Read
)

func (d Dir) String() string {
	if d == Publish {
		return "publish"
	}
	return "read"
}

// DefaultRetention is how long a topic is kept when nothing says otherwise.
const DefaultRetention = 30 * 24 * time.Hour

// Patterns are topic patterns: "*" (every topic), a prefix "job.*" (every
// topic under job, at any depth), or one topic exactly.
type Patterns []string

// Match says whether any of the patterns covers the topic.
func (p Patterns) Match(kind string) bool { return offices.TopicMatchAny(p, kind) }

// Topics is what one peer or seat may publish and read.
type Topics struct {
	Publish Patterns
	Read    Patterns
}

// Rules are a region's topic rules: who may send what into its node and
// read what out of it. Everything defaults to deny. The node checks them on
// both sides: before an event leaves for a peer (the peer's Read) and when
// one arrives from a peer (its Publish), so a peer's own rules can't widen
// this region's.
type Rules struct {
	Peers map[string]Topics // by region name
	Seats map[string]Topics // by Unix user
	// PrivateTopics never leave this region, and no peer may publish into
	// them; local seats use them like any other.
	PrivateTopics Patterns
	// Retain is a retention per pattern; the most specific pattern that
	// covers a topic wins, then Default, then DefaultRetention.
	Retain  map[string]time.Duration
	Default time.Duration
}

// RulesOf reads a region's rules from its (already checked) region file.
func RulesOf(r *offices.Region) Rules {
	out := Rules{
		Peers:         map[string]Topics{},
		Seats:         map[string]Topics{},
		PrivateTopics: Patterns(r.Bus.Private),
		Retain:        map[string]time.Duration{},
		Default:       DefaultRetention,
	}
	if d, err := time.ParseDuration(r.Bus.Retention); err == nil && d > 0 {
		out.Default = d
	}
	for pat, v := range r.Bus.Retain {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			out.Retain[pat] = d
		}
	}
	for name, p := range r.Peers {
		out.Peers[name] = Topics{Publish: Patterns(p.MayPublish), Read: Patterns(p.MayRead)}
	}
	for user, s := range r.Bus.Seats {
		out.Seats[user] = Topics{Publish: Patterns(s.Publish), Read: Patterns(s.Read)}
	}
	return out
}

// Private says whether a topic never leaves this region.
func (r Rules) Private(kind string) bool { return r.PrivateTopics.Match(kind) }

// Allowed says whether an event of this kind may go this way between this
// region and a peer. A private topic never goes either way.
func (r Rules) Allowed(peer, kind string, dir Dir) bool {
	t, ok := r.Peers[peer]
	if !ok || r.Private(kind) {
		return false
	}
	if dir == Publish {
		return t.Publish.Match(kind)
	}
	return t.Read.Match(kind)
}

// SeatAllowed says whether a local seat (its Unix user) may publish or read
// an event of this kind. Private topics are the region's own seats' to use.
func (r Rules) SeatAllowed(user, kind string, dir Dir) bool {
	t, ok := r.Seats[user]
	if !ok {
		return false
	}
	if dir == Publish {
		return t.Publish.Match(kind)
	}
	return t.Read.Match(kind)
}

// Retention is how long events of this kind are kept.
func (r Rules) Retention(kind string) time.Duration {
	best, d := -1, r.Default
	for pat, v := range r.Retain {
		if s := offices.TopicSpecificity(pat); offices.TopicMatch(pat, kind) && s > best {
			best, d = s, v
		}
	}
	if d <= 0 {
		return DefaultRetention
	}
	return d
}
