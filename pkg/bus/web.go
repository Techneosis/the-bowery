package bus

// The private projection, served to its owner's browser: the one way a
// region's private records reach a page. The board (a web app on another
// origin) reads a snapshot and a stream of changes from here, and writes a
// private record here, straight to this node over the tailnet; nothing
// private passes through the board's server.
//
//	GET  /private/snapshot  {"snapshot": <PrivateState, its cursor the node's heads>, "csrf": <token>}
//	GET  /private/stream    server-sent events: "snapshot" (or "ready", for a reader whose
//	                        ?cursor= or Last-Event-ID is current), then a "change" per change,
//	                        {"cursor", "put": {list: [entries]}, "drop": {list: [keys]}};
//	                        every event's id is the cursor it brings the reader to
//	POST /private/publish   {"kind": "private.card", "data": {…}} with X-CSRF-Token:
//	                        published as the configured seat; {"ref", "cursor"}
//
// Who may ask is settled before anything is read, in this order:
//   - the Host is the node's own name (a rebound name is 421);
//   - an Origin, if the request has one, is the board's exactly; a write
//     must have one;
//   - the tailnet says the caller is the owner's login, on an untagged
//     device: a peer region's node, a server or anyone else is 403;
//   - a write carries a CSRF token this node minted for that login, at
//     most twelve hours old.
//
// CORS answers the board's origin only, never "*", and never allows
// credentials: whois is the identity, so there's no cookie to send.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WhoIs is what the tailnet says about a caller.
type WhoIs struct {
	Login  string // the user's login name
	Tagged bool   // the device is tagged (a server, a peer's node): it belongs to no person
}

// WebConfig is the private web endpoint's configuration.
type WebConfig struct {
	Host   string // the node's tailnet name, the only Host served
	Owner  string // the one tailnet login served
	Origin string // the board's origin (https://host[:port]): CORS and the Origin check
	Seat   string // the local seat a browser reads and publishes as
	// WhoIs asks the tailnet who is at a request's remote address.
	WhoIs func(ctx context.Context, remoteAddr string) (WhoIs, error)
	// Secret keys the CSRF tokens (nil: random, so a restart retires them).
	Secret []byte
	Now    func() time.Time
	// MaxStreams is how many streams may be open at once (default 32).
	MaxStreams int
}

// CSRFTTL is how long a minted token is good for.
const CSRFTTL = 12 * time.Hour

const (
	maxWebBody = 64 << 10
	keepAlive  = 25 * time.Second
	whoisTTL   = 30 * time.Second
)

type web struct {
	n       *Node
	cfg     WebConfig
	streams chan struct{}

	mu    sync.Mutex
	known map[string]whoisSeen // by remote IP
}

type whoisSeen struct {
	at  time.Time
	who WhoIs
}

// WebHandler is the node's private web endpoint, configured; serve it over
// TLS on the tailnet address only.
func (n *Node) WebHandler(cfg WebConfig) (http.Handler, error) {
	if cfg.Host == "" || strings.ContainsAny(cfg.Host, ":/ ") {
		return nil, fmt.Errorf("bus web: host %q isn't a host name", cfg.Host)
	}
	if cfg.Owner == "" || strings.ContainsAny(cfg.Owner, " \t") {
		return nil, fmt.Errorf("bus web: owner %q isn't a tailnet login", cfg.Owner)
	}
	if err := CheckOrigin(cfg.Origin); err != nil {
		return nil, fmt.Errorf("bus web: %v", err)
	}
	if !ValidName(cfg.Seat) {
		return nil, fmt.Errorf("bus web: seat %q isn't a seat name", cfg.Seat)
	}
	if cfg.WhoIs == nil {
		return nil, errors.New("bus web: no whois: the tailnet says who's asking")
	}
	if len(cfg.Secret) == 0 {
		cfg.Secret = make([]byte, 32)
		if _, err := rand.Read(cfg.Secret); err != nil {
			return nil, err
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 32
	}
	return &web{n: n, cfg: cfg, streams: make(chan struct{}, cfg.MaxStreams), known: map[string]whoisSeen{}}, nil
}

// CheckOrigin says whether o is an origin a board can have: https, a host,
// maybe a port, and nothing more.
func CheckOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(o, "/") {
		return fmt.Errorf("origin %q isn't an origin like https://board.example.ts.net", o)
	}
	return nil
}

func webError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Write(append(b, '\n'))
}

func (h *web) whois(ctx context.Context, addr string) (WhoIs, error) {
	ip := addr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		ip = host
	}
	now := h.cfg.Now()
	h.mu.Lock()
	seen, ok := h.known[ip]
	h.mu.Unlock()
	if ok && now.Sub(seen.at) < whoisTTL && now.Sub(seen.at) >= 0 {
		return seen.who, nil
	}
	who, err := h.cfg.WhoIs(ctx, addr)
	if err != nil || !h.owner(who) {
		// Only the owner is kept: anyone else is asked about again.
		return who, err
	}
	h.mu.Lock()
	if len(h.known) > 256 {
		h.known = map[string]whoisSeen{}
	}
	h.known[ip] = whoisSeen{now, who}
	h.mu.Unlock()
	return who, nil
}

// owner: the owner's login, on a device that is a person's (not tagged).
func (h *web) owner(who WhoIs) bool {
	return !who.Tagged && who.Login != "" && who.Login == h.cfg.Owner
}

func (h *web) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Vary", "Origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	host := r.Host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		host = hp
	}
	if !strings.EqualFold(strings.TrimSuffix(host, "."), h.cfg.Host) {
		webError(w, http.StatusMisdirectedRequest, "this node serves "+h.cfg.Host)
		return
	}
	origin := r.Header.Get("Origin")
	if _, sent := r.Header["Origin"]; sent && origin != h.cfg.Origin {
		webError(w, http.StatusForbidden, "not from "+h.cfg.Origin)
		return
	}
	who, err := h.whois(r.Context(), r.RemoteAddr)
	switch {
	case err != nil:
		webError(w, http.StatusForbidden, "the tailnet can't say who's asking")
		return
	case !h.owner(who):
		webError(w, http.StatusForbidden, "this node serves its owner's devices only")
		return
	}
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", h.cfg.Origin)
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-CSRF-Token, Last-Event-ID")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	route := map[string]string{"/private/snapshot": "GET", "/private/stream": "GET", "/private/publish": "POST"}
	want, ok := route[r.URL.Path]
	if !ok {
		webError(w, http.StatusNotFound, "no such path: /private/snapshot, /private/stream or /private/publish")
		return
	}
	if r.Method != want {
		w.Header().Set("Allow", want)
		webError(w, http.StatusMethodNotAllowed, r.URL.Path+" takes "+want)
		return
	}
	switch r.URL.Path {
	case "/private/snapshot":
		h.snapshot(w, who)
	case "/private/stream":
		h.stream(w, r)
	case "/private/publish":
		if origin == "" {
			webError(w, http.StatusForbidden, "a write says its Origin")
			return
		}
		if !h.tokenOK(r.Header.Get("X-CSRF-Token"), who.Login) {
			webError(w, http.StatusForbidden, "no good X-CSRF-Token: take a fresh one from /private/snapshot")
			return
		}
		h.publish(w, r)
	}
}

// ---------------------------------------------------------------- csrf

func (h *web) mac(login string, exp int64) string {
	m := hmac.New(sha256.New, h.cfg.Secret)
	fmt.Fprintf(m, "bus-web-csrf\x00%s\x00%d", login, exp)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (h *web) token(login string) string {
	exp := h.cfg.Now().Add(CSRFTTL).Unix()
	return strconv.FormatInt(exp, 10) + "." + h.mac(login, exp)
}

func (h *web) tokenOK(tok, login string) bool {
	e, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(e, 10, 64)
	if err != nil || h.cfg.Now().Unix() >= exp {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sig), []byte(h.mac(login, exp))) == 1
}

// ---------------------------------------------------------------- reads

// fold reads the seat's private records from scan on into f; scan ends at
// the node's heads.
func (h *web) fold(f *PrivateFold, scan Cursor) error {
	const batch = 256
	for {
		recs, err := h.n.log.SinceKept(scan, batch)
		if err != nil {
			return err
		}
		for _, r := range recs {
			scan.Advance(r.Ref())
			k := r.Event.Kind
			if strings.HasPrefix(k, "private.") && h.n.cfg.Rules.SeatAllowed(h.cfg.Seat, k, Read) {
				f.Apply(r)
			}
		}
		if len(recs) < batch {
			return nil
		}
	}
}

func (h *web) state(f *PrivateFold, scan Cursor) PrivateState {
	s := f.State()
	s.Cursor = scan.String()
	return s
}

func writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(append(body, '\n'))
}

func (h *web) snapshot(w http.ResponseWriter, who WhoIs) {
	f, scan := NewPrivateFold(), Cursor{}
	if err := h.fold(f, scan); err != nil {
		webError(w, http.StatusInternalServerError, err.Error())
		return
	}
	b, err := h.state(f, scan).Bytes()
	if err != nil {
		webError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tok, _ := json.Marshal(h.token(who.Login))
	writeJSON(w, []byte(`{"snapshot":`+string(b)+`,"csrf":`+string(tok)+`}`))
}

// entries is a snapshot's entries by list and key.
func entries(s PrivateState) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, l := range PrivateLists {
		out[l] = map[string]string{}
		for _, e := range s.Lists[l] {
			out[l][e.Key] = string(e.V)
		}
	}
	return out
}

type webChange struct {
	Cursor string                    `json:"cursor"`
	Put    map[string][]PrivateEntry `json:"put"`
	Drop   map[string][]string       `json:"drop"`
}

// change is what changed from was to now, or false if nothing did.
func change(was map[string]map[string]string, now PrivateState) (webChange, bool) {
	c := webChange{Cursor: now.Cursor, Put: map[string][]PrivateEntry{}, Drop: map[string][]string{}}
	any := false
	for _, l := range PrivateLists {
		in := map[string]bool{}
		for _, e := range now.Lists[l] {
			in[e.Key] = true
			if was[l][e.Key] != string(e.V) {
				c.Put[l] = append(c.Put[l], e)
				any = true
			}
		}
		for _, e := range sortedKeys(was[l]) {
			if !in[e] {
				c.Drop[l] = append(c.Drop[l], e)
				any = true
			}
		}
	}
	return c, any
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (h *web) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		webError(w, http.StatusInternalServerError, "this server can't stream")
		return
	}
	select {
	case h.streams <- struct{}{}:
		defer func() { <-h.streams }()
	default:
		webError(w, http.StatusServiceUnavailable, "too many streams open")
		return
	}
	from := r.URL.Query().Get("cursor")
	if from == "" {
		from = r.Header.Get("Last-Event-ID")
	}
	if _, err := ParseCursor(from); err != nil {
		webError(w, http.StatusBadRequest, err.Error())
		return
	}
	f, scan := NewPrivateFold(), Cursor{}
	wake := h.n.waiter()
	if err := h.fold(f, scan); err != nil {
		webError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event, id string, data []byte) bool {
		if _, err := fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n", event, id, data); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	s := h.state(f, scan)
	if from != "" && from == s.Cursor {
		b, _ := json.Marshal(map[string]string{"cursor": s.Cursor})
		if !send("ready", s.Cursor, b) {
			return
		}
	} else {
		b, err := s.Bytes()
		if err != nil || !send("snapshot", s.Cursor, b) {
			return
		}
	}
	was := entries(s)
	ctx := r.Context()
	ping := time.NewTicker(keepAlive)
	defer ping.Stop()
	for {
		t := time.NewTimer(Backstop)
		select {
		case <-wake:
		case <-t.C:
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				t.Stop()
				return
			}
			fl.Flush()
		case <-ctx.Done():
			t.Stop()
			return
		}
		t.Stop()
		wake = h.n.waiter()
		if err := h.fold(f, scan); err != nil {
			b, _ := json.Marshal(map[string]string{"error": err.Error()})
			send("error", scan.String(), b)
			return
		}
		now := h.state(f, scan)
		c, changed := change(was, now)
		if !changed {
			continue
		}
		b, err := marshal(c)
		if err != nil || !send("change", now.Cursor, b) {
			return
		}
		was = entries(now)
	}
}

// ---------------------------------------------------------------- writes

func (h *web) publish(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebBody+1))
	if err != nil {
		webError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(body) > maxWebBody {
		webError(w, http.StatusRequestEntityTooLarge, "a record is at most 64 KiB")
		return
	}
	var in struct {
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		webError(w, http.StatusBadRequest, "the body is {\"kind\": \"private.card\", \"data\": {…}}: "+err.Error())
		return
	}
	if err := CheckPrivate(in.Kind, in.Data); err != nil {
		webError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := h.n.Publish(h.cfg.Seat, Publication{Kind: in.Kind, Data: in.Data})
	if err != nil {
		webError(w, http.StatusForbidden, err.Error())
		return
	}
	b, _ := json.Marshal(map[string]string{"ref": rec.Ref().String(), "cursor": h.n.log.Heads().String()})
	writeJSON(w, b)
}
