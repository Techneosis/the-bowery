package offices

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The bus's topic rules live in the region file, beside the placement
// rules, so one edit grants or revokes both. See pkg/bus for what they mean.

// BusConfig is the region file's `[bus]`.
type BusConfig struct {
	// Retention is how long a topic's events are kept (default 720h).
	Retention string `toml:"retention"`
	// Retain is a retention per topic pattern; the most specific wins.
	Retain map[string]string `toml:"retain"`
	// Private topics never leave this region, whatever a peer's may_read
	// says, and no peer may publish into them.
	Private []string `toml:"private"`
	// Seats are the topics this region's local seats may publish and read,
	// keyed by Unix user.
	Seats map[string]BusSeat `toml:"seats"`
	// Web serves the private projection to the owner's browser (pkg/bus
	// web.go), if it's set.
	Web *BusWeb `toml:"web"`
}

// BusWeb is `[bus.web]`: the node's HTTPS endpoint on the tailnet, for its
// owner's devices only.
type BusWeb struct {
	Listen string `toml:"listen"` // the tailnet address and port: an IP, never 0.0.0.0
	Host   string `toml:"host"`   // the node's tailnet name: its certificate's, and the only Host served
	Owner  string `toml:"owner"`  // the one tailnet login served (whois)
	Origin string `toml:"origin"` // the board's origin, https://host[:port]: CORS and the Origin check
	Seat   string `toml:"seat"`   // the local seat (a [bus.seats] user) a browser reads and publishes as
	// Tailscaled is tailscaled's socket (default /run/tailscale/tailscaled.sock).
	Tailscaled string `toml:"tailscaled"`
}

func checkWeb(file string, b BusConfig) error {
	w := b.Web
	host, port, err := net.SplitHostPort(w.Listen)
	ip, perr := netip.ParseAddr(host)
	if err != nil || port == "" || perr != nil || ip.IsUnspecified() {
		return fmt.Errorf("%s: bus.web.listen: %q isn't an address and port on the tailnet (like 100.64.0.1:7450; never every address)", file, w.Listen)
	}
	if w.Host == "" || strings.ContainsAny(w.Host, ":/ ") {
		return fmt.Errorf("%s: bus.web.host: %q isn't the node's tailnet name", file, w.Host)
	}
	if w.Owner == "" || strings.ContainsAny(w.Owner, " \t") {
		return fmt.Errorf("%s: bus.web.owner: %q isn't a tailnet login", file, w.Owner)
	}
	if u, err := url.Parse(w.Origin); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s: bus.web.origin: %q isn't one origin like https://board.example.ts.net", file, w.Origin)
	}
	if _, ok := b.Seats[w.Seat]; !ok {
		return fmt.Errorf("%s: bus.web.seat: %s has no [bus.seats.%s]: the browser reads and publishes as a seat", file, w.Seat, w.Seat)
	}
	return nil
}

// BusSeat is one local seat's topics.
type BusSeat struct {
	Publish []string `toml:"publish"`
	Read    []string `toml:"read"`
}

var (
	topicHead = `[a-z][a-z0-9_]{0,31}`
	topicPart = `[a-z][a-z0-9_-]{0,31}`
	// A topic is two to four dotted parts; a pattern is "*", one to three
	// parts and ".*", or a topic.
	topicRE             = regexp.MustCompile(`^` + topicHead + `(\.` + topicPart + `){1,3}$`)
	prefixRE            = regexp.MustCompile(`^` + topicHead + `(\.` + topicPart + `){0,2}\.\*$`)
	unixUser            = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	DefaultBusRetention = 30 * 24 * time.Hour
)

// ValidTopic is a topic like job.started: two to four parts, no wildcards.
func ValidTopic(k string) bool { return topicRE.MatchString(k) }

// ValidTopicPattern is "*" (every topic), a prefix like "job.*" (every
// topic under job, at any depth), or one topic exactly.
func ValidTopicPattern(p string) bool {
	return p == "*" || prefixRE.MatchString(p) || topicRE.MatchString(p)
}

// TopicMatch says whether a valid pattern covers a topic. A prefix is whole
// parts: "job.*" covers job.started and job.started.late, not jobs.started
// or job itself.
func TopicMatch(pat, topic string) bool {
	if pat == "*" {
		return true
	}
	if pre, ok := strings.CutSuffix(pat, "*"); ok {
		return strings.HasPrefix(topic, pre)
	}
	return pat == topic
}

// TopicMatchAny says whether any of the patterns covers the topic.
func TopicMatchAny(pats []string, topic string) bool {
	for _, p := range pats {
		if TopicMatch(p, topic) {
			return true
		}
	}
	return false
}

// TopicSpecificity orders the patterns that cover one topic: a topic named
// exactly beats any prefix, a longer prefix beats a shorter one, and "*"
// is last.
func TopicSpecificity(pat string) int {
	switch {
	case pat == "*":
		return 0
	case strings.HasSuffix(pat, "*"):
		return strings.Count(pat, ".")
	default:
		return 100
	}
}

func checkTopics(file, where string, pats []string) error {
	for _, p := range pats {
		if !ValidTopicPattern(p) {
			return fmt.Errorf(`%s: %s: %q isn't a topic pattern ("*", "job.*" or job.started)`, file, where, p)
		}
	}
	return nil
}

func checkRetention(file, where, v string) error {
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("%s: %s: %v", file, where, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s: %s: %q isn't a time to keep events for", file, where, v)
	}
	return nil
}

// checkBus checks the region's `[bus]` and its peers' topic lists.
func checkBus(file string, r *Region) error {
	b := r.Bus
	if b.Retention != "" {
		if err := checkRetention(file, "bus.retention", b.Retention); err != nil {
			return err
		}
	}
	for pat, v := range b.Retain {
		if !ValidTopicPattern(pat) {
			return fmt.Errorf("%s: bus.retain: %q isn't a topic pattern", file, pat)
		}
		if err := checkRetention(file, fmt.Sprintf("bus.retain.%q", pat), v); err != nil {
			return err
		}
	}
	if err := checkTopics(file, "bus.private", b.Private); err != nil {
		return err
	}
	for _, p := range b.Private {
		if p == "*" {
			return fmt.Errorf(`%s: bus.private: "*" would keep every topic in, and the bus would carry nothing`, file)
		}
	}
	if b.Web != nil {
		if err := checkWeb(file, b); err != nil {
			return err
		}
	}
	for user, s := range b.Seats {
		if !unixUser.MatchString(user) {
			return fmt.Errorf("%s: bus.seats.%s: not a Unix user name", file, user)
		}
		if err := checkTopics(file, "bus.seats."+user+".publish", s.Publish); err != nil {
			return err
		}
		if err := checkTopics(file, "bus.seats."+user+".read", s.Read); err != nil {
			return err
		}
	}
	for name, p := range r.Peers {
		if err := checkTopics(file, "peers."+name+".may_publish", p.MayPublish); err != nil {
			return err
		}
		if err := checkTopics(file, "peers."+name+".may_read", p.MayRead); err != nil {
			return err
		}
	}
	return nil
}
