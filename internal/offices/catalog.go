package offices

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CatalogDoc is what a region publishes to one peer about one machine's
// offices (spec §6): only the offices that peer may place into, with their
// holdings, state and office-wide verdicts, signed with the region's key.
// It is advisory: the receiving office decides.
type CatalogDoc struct {
	Region      string     `json:"region"`
	Machine     string     `json:"machine"`
	For         string     `json:"for"` // the peer it was published to
	PublishedAt string     `json:"published_at"`
	ValidUntil  string     `json:"valid_until"`
	Vocab       Vocab      `json:"vocab"`
	Offices     []*Catalog `json:"offices"`
}

// Vocab is the publishing region's own words, which a peer's resolver needs
// to match a job against its offices.
type Vocab struct {
	Priorities      []string `json:"priorities"`
	ReservedEffects []string `json:"reserved_effects"`
	MaxExpires      string   `json:"max_expires,omitempty"`
	Timezone        string   `json:"timezone"`
}

// Region is the vocabulary as a region, for Mismatch.
func (v Vocab) Region(name string) *Region {
	return &Region{Region: name, Priorities: v.Priorities, ReservedEffects: v.ReservedEffects, MaxExpires: v.MaxExpires,
		Timezone: v.Timezone}
}

// ForPeer is an office's catalog entry as a peer sees it: nothing about
// other jobs, and no local paths.
func (c *Catalog) ForPeer() *Catalog {
	out := *c
	out.Jobs, out.BoxPath = []Running{}, ""
	return &out
}

const catalogFence = "```catalog"

// SignCatalog writes a catalog document and the region's signature over
// its bytes, in namespace offices-catalog.
func SignCatalog(doc *CatalogDoc, key PrivateKey) ([]byte, error) {
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	msg := append(append([]byte(catalogFence+"\n"), body...), []byte("\n```\n")...)
	sig, err := SignWith(key, NSCatalog, msg)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Write(msg)
	b.WriteString("```signature region\n")
	b.Write(bytes.TrimRight(sig, "\n"))
	b.WriteString("\n```\n")
	return b.Bytes(), nil
}

// ErrNoLink is a catalog that isn't signed by the key this region pins for
// its publisher: a mismatch means no link (spec §3.1).
var ErrNoLink = errors.New("no link")

// ErrStale is a verified catalog past its valid_until.
var ErrStale = errors.New("stale")

// ErrFuture is a verified catalog dated ahead of this machine's clock by
// more than ClockSkew: like a gate dated in the future, it would vouch for a
// moment that hasn't come, so it is refused, not taken as fresh.
var ErrFuture = errors.New("dated in the future")

// ClockSkew is how far ahead of this machine's clock another machine's or
// region's time may be before it is distrusted (a gate's, a catalog's, a
// peer's signing time).
const ClockSkew = gateSkew

// OpenCatalog verifies a received catalog against the publisher's pinned
// key and checks that it is from that region, for this one, and still
// valid. The document comes back with ErrStale, so its age can be said.
func OpenCatalog(b []byte, pin string, from, me string, now time.Time) (*CatalogDoc, error) {
	s := string(b)
	if !strings.HasPrefix(s, catalogFence+"\n") {
		return nil, fmt.Errorf("%w: not a catalog", ErrNoLink)
	}
	end := strings.Index(s, "\n```\n")
	if end < 0 {
		return nil, fmt.Errorf("%w: not a catalog", ErrNoLink)
	}
	msg := []byte(s[:end+len("\n```\n")])
	rest := s[end+len("\n```\n"):]
	sigBody, ok := strings.CutPrefix(rest, "```signature region\n")
	if !ok || !strings.HasSuffix(sigBody, "\n```\n") {
		return nil, fmt.Errorf("%w: the catalog carries no region signature", ErrNoLink)
	}
	pinKey, err := ParsePublicKey(pin)
	if err != nil {
		return nil, fmt.Errorf("%w: region %s's pinned key isn't usable", ErrNoLink, from)
	}
	key, err := VerifySignature([]byte(strings.TrimSuffix(sigBody, "```\n")), NSCatalog, msg)
	if err != nil {
		return nil, fmt.Errorf("%w: region %s's catalog signature doesn't verify", ErrNoLink, from)
	}
	if !key.Equal(pinKey) {
		return nil, fmt.Errorf("%w: region %s's catalog is signed by %s, not by the key pinned for it", ErrNoLink, from, key.Fingerprint())
	}
	doc := &CatalogDoc{}
	dec := json.NewDecoder(bytes.NewReader(msg[len(catalogFence)+1 : len(msg)-len("\n```\n")]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(doc); err != nil {
		return nil, fmt.Errorf("%w: region %s's catalog doesn't decode: %v", ErrNoLink, from, err)
	}
	if doc.Region != from || doc.For != me {
		return nil, fmt.Errorf("%w: a catalog of region %q for %q, not of %s for %s", ErrNoLink, doc.Region, doc.For, from, me)
	}
	for _, o := range doc.Offices {
		if o == nil || o.Region != from || !NameRE.MatchString(o.Office) {
			return nil, fmt.Errorf("%w: region %s's catalog lists an office that isn't its own", ErrNoLink, from)
		}
	}
	pa, err := time.Parse(time.RFC3339Nano, doc.PublishedAt)
	if err != nil {
		return nil, fmt.Errorf("%w: region %s's catalog has no publishing time", ErrNoLink, from)
	}
	if ahead := pa.Sub(now); ahead > ClockSkew {
		return doc, fmt.Errorf("%w: region %s's catalog is published %s ahead of this clock", ErrFuture, from, ahead.Round(time.Second))
	}
	vu, err := time.Parse(time.RFC3339Nano, doc.ValidUntil)
	if err != nil || !vu.After(pa) || !now.Before(vu) {
		return doc, ErrStale
	}
	return doc, nil
}
