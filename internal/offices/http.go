package offices

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The HTTP transport: each machine's office host serves one endpoint
// (`office serve`), and a peer's `address` is its URL. A placement is a POST
// into an office's box, a reply a POST into an asker's, and a catalog is
// fetched by the peer it was published for. The endpoint stores what it is
// given in the same on-disk boxes the directory transport uses, so the pull
// job and its long-poll are unchanged: the box's directory watch (inotify,
// kqueue) wakes the loop the moment the file lands.
//
// Identity is still only the signature. The endpoint checks shape (a size
// cap, one content type, a name, a letter that decodes), never who is
// asking: anything it stores is judged by the office like any letter.

const (
	// LetterType is the one content type the endpoint takes.
	LetterType = "text/plain"
	// MaxPlacement and MaxReply cap a request body: a placement is a few
	// KiB, a reply one line.
	MaxPlacement = 64 << 10
	MaxReply     = 4 << 10
	// MaxPending is how many placements a box may hold before the endpoint
	// refuses more (each is judged on every pass).
	MaxPending = 256
	// MachineHeader names the machine a served catalog is from.
	MachineHeader = "X-Office-Machine"
)

// HTTPTransport is the client side: bowery's Send and Catalogs, the office's
// Reply and Publish. Its own region's address ("") is the local boxes, as
// for the directory transport.
type HTTPTransport struct {
	Dir     DirTransport
	Client  *http.Client
	Tries   int           // attempts for a POST (default 3)
	Backoff time.Duration // before the second attempt, doubled after (default 200ms)
}

func NewHTTPTransport(l Layout, poll time.Duration) *HTTPTransport {
	return &HTTPTransport{
		Dir: DirTransport{L: l, Poll: poll},
		Client: &http.Client{
			Timeout: 5 * time.Second,
			// A redirect goes somewhere the peer entry doesn't name.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Tries:   3,
		Backoff: 200 * time.Millisecond,
	}
}

// ParseAddress is an http transport address: an absolute http or https URL
// with a host, and no user, query or fragment. A path is a prefix.
func ParseAddress(addr string) (*url.URL, error) {
	u, err := url.Parse(addr)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("the http transport's address is an http:// or https:// URL with a host and no user, query or fragment (got %q)", addr)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func endpoint(addr string, parts ...string) (string, error) {
	u, err := ParseAddress(addr)
	if err != nil {
		return "", err
	}
	for _, p := range parts {
		u = u.JoinPath(p)
	}
	return u.String(), nil
}

func (t *HTTPTransport) Send(addr, box, id string, body []byte) error {
	if addr == "" {
		return t.Dir.Send(addr, box, id, body)
	}
	if !safeName(box) {
		return fmt.Errorf("%q isn't a box name", box)
	}
	if !IDRE.MatchString(id) {
		return fmt.Errorf("letter id %q", id)
	}
	u, err := endpoint(addr, "placements", box)
	if err != nil {
		return err
	}
	return t.post(u, body)
}

func (t *HTTPTransport) Reply(addr, asker, id string, body []byte) error {
	if addr == "" {
		return t.Dir.Reply(addr, asker, id, body)
	}
	if !safeName(asker) {
		return fmt.Errorf("asker %q isn't a box name", asker)
	}
	if !IDRE.MatchString(id) {
		return fmt.Errorf("letter id %q", id)
	}
	u, err := endpoint(addr, "replies", asker)
	if err != nil {
		return err
	}
	return t.post(u+"?id="+url.QueryEscape(id), body)
}

// post sends one letter, trying again on a network error or a 5xx. A 409 on
// a later try is the earlier try's letter, already in the box.
func (t *HTTPTransport) post(u string, body []byte) error {
	tries := t.Tries
	if tries < 1 {
		tries = 1
	}
	wait := t.Backoff
	var last error
	for i := 0; i < tries; i++ {
		if i > 0 {
			time.Sleep(wait)
			wait *= 2
		}
		req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", LetterType+"; charset=utf-8")
		resp, err := t.Client.Do(req)
		if err != nil {
			last = err
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusCreated:
			return nil
		case resp.StatusCode == http.StatusConflict && i > 0:
			return nil
		case resp.StatusCode >= 500:
			last = fmt.Errorf("%s: %s %s", u, resp.Status, strings.TrimSpace(string(msg)))
			continue
		default:
			return fmt.Errorf("%s: %s %s", u, resp.Status, strings.TrimSpace(string(msg)))
		}
	}
	return last
}

// Publish keeps the catalog for peer to fetch (GET /catalog/<peer> on this
// machine's endpoint). addr isn't used: the peer comes for it.
func (t *HTTPTransport) Publish(peer, _, from, machine string, body []byte) error {
	if !NameRE.MatchString(peer) || !NameRE.MatchString(from) || !LabelRE.MatchString(machine) || strings.ContainsAny(machine, "/") {
		return fmt.Errorf("peer %q, region %q or machine %q isn't a name", peer, from, machine)
	}
	dir := t.Dir.L.Published(peer)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return WriteAtomic(filepath.Join(dir, machine+".catalog"), body, 0o644)
}

// Catalogs fetches the catalog region from's endpoint keeps for region
// self. None published yet (404) is no catalogs, not an error.
func (t *HTTPTransport) Catalogs(from, addr, self string) ([]Received, error) {
	if !NameRE.MatchString(from) || !NameRE.MatchString(self) {
		return nil, fmt.Errorf("%q or %q isn't a region name", from, self)
	}
	if addr == "" {
		return t.Dir.Catalogs(from, addr, self)
	}
	u, err := endpoint(addr, "catalog", self)
	if err != nil {
		return nil, err
	}
	var resp *http.Response
	for i := 0; i < 2; i++ {
		if resp, err = t.Client.Get(u); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLetter+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxLetter {
		return nil, fmt.Errorf("%s: the catalog is over %d bytes", u, maxLetter)
	}
	m := resp.Header.Get(MachineHeader)
	if !LabelRE.MatchString(m) || strings.ContainsAny(m, "/") {
		return nil, fmt.Errorf("%s: no machine name in %s", u, MachineHeader)
	}
	r := Received{Machine: m, Body: body, At: time.Now()}
	if lm, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		r.At = lm
	}
	return []Received{r}, nil
}

func (t *HTTPTransport) Inbox(box string) Inbox { return t.Dir.Inbox(box) }

// ---------------------------------------------------------------- the endpoint

// Server is `office serve`'s handler for one machine's layout. It reads the
// region and machine files on every request, so an office added to the
// region file is served without a restart. Nothing is served but its three
// paths, and nothing is ever listed.
type Server struct {
	L Layout
	// Log, if set, is told of every request the endpoint refuses or stores.
	Log func(format string, a ...any)
}

func (s *Server) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log(format, a...)
	}
}

// NewHTTPServer is the endpoint's http.Server, with the limits a public
// listener needs: header and body timeouts and a header cap.
func NewHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, why string) {
		s.logf("%s %s: %d %s", r.Method, r.URL.EscapedPath(), code, why)
		http.Error(w, why, code)
	}
	// The raw path, so an escaped "/" or ".." can't pass as a name.
	parts := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	if len(parts) != 2 || parts[1] == "" {
		fail(http.StatusNotFound, "not found")
		return
	}
	route, name := parts[0], parts[1]
	if route == "bus" && name == "peer" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			fail(http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.busPeer(w, r, fail)
		return
	}
	want := http.MethodPost
	if route == "catalog" {
		want = http.MethodGet
	}
	switch route {
	case "placements", "replies", "catalog":
	default:
		fail(http.StatusNotFound, "not found")
		return
	}
	if r.Method != want {
		w.Header().Set("Allow", want)
		fail(http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// A box is matched against this machine's own boxes below; an asker
	// and a peer are names. An escaped segment ("%2F", "%2E%2E") never
	// passes either.
	ok := NameRE.MatchString(name)
	if route != "catalog" {
		ok = safeName(name)
	}
	if !ok {
		fail(http.StatusNotFound, "not found")
		return
	}
	reg, err := LoadRegion(s.L)
	if err != nil {
		fail(http.StatusInternalServerError, "the region file can't be read")
		s.logf("region file: %v", err)
		return
	}
	m, err := LoadMachine(s.L)
	if err != nil {
		fail(http.StatusInternalServerError, "the machine file can't be read")
		s.logf("machine file: %v", err)
		return
	}
	switch route {
	case "catalog":
		s.catalog(w, r, name, m.Machine, fail)
	case "placements":
		box := ""
		for _, o := range reg.Offices {
			if o.Machine == m.Machine && BoxName(reg, o.Name) == name {
				box = name
			}
		}
		if box == "" {
			fail(http.StatusNotFound, "not found")
			return
		}
		s.store(w, r, s.L.BoxDir(box), MaxPlacement, true, fail)
	case "replies":
		// Only to an asker's box that is already there: a stranger can't
		// make this machine grow a directory per name.
		dir := s.L.BoxDir("asker-" + name)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			fail(http.StatusNotFound, "not found")
			return
		}
		s.store(w, r, dir, MaxReply, false, fail)
	}
}

// MaxBusCall caps a peer's bus call (pkg/bus's MaxPeerCall), and BusWait
// is how long the endpoint lets one take: a pull long-polls up to its
// MaxWait.
const (
	MaxBusCall = 8 << 20
	BusWait    = 40 * time.Second
)

// busPeer hands a peer region's bus call to this machine's bus node, on
// its socket, and sends back the node's answer. The node checks the call's
// signature: the endpoint checks only its shape.
func (s *Server) busPeer(w http.ResponseWriter, r *http.Request, fail func(int, string)) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		fail(http.StatusUnsupportedMediaType, "a bus call is application/json")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBusCall+1))
	if err != nil {
		fail(http.StatusBadRequest, "the body can't be read")
		return
	}
	if len(body) > MaxBusCall {
		fail(http.StatusRequestEntityTooLarge, "too large")
		return
	}
	var env json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil || len(env) == 0 || env[0] != '{' {
		fail(http.StatusBadRequest, "a bus call is a JSON object")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(BusWait + 5*time.Second))
	var d net.Dialer
	ctx, cancel := context.WithTimeout(r.Context(), BusWait)
	defer cancel()
	c, err := d.DialContext(ctx, "unix", s.L.BusSocket())
	if err != nil {
		fail(http.StatusServiceUnavailable, "no bus node on this machine")
		s.logf("bus node: %v", err)
		return
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Now()) })
	defer stop()
	req, _ := json.Marshal(struct {
		Op   string          `json:"op"`
		Peer json.RawMessage `json:"peer"`
	}{"peer", env})
	if _, err := c.Write(append(req, '\n')); err != nil {
		fail(http.StatusBadGateway, "the bus node didn't take the call")
		return
	}
	line, err := io.ReadAll(io.LimitReader(c, MaxBusCall+1))
	if err != nil || len(line) > MaxBusCall {
		fail(http.StatusBadGateway, "the bus node didn't answer")
		return
	}
	var rep struct {
		Error     string          `json:"error"`
		PeerReply json.RawMessage `json:"peer_reply"`
	}
	if err := json.Unmarshal(line, &rep); err != nil {
		fail(http.StatusBadGateway, "the bus node's answer isn't one")
		return
	}
	out := rep.PeerReply
	if len(out) == 0 {
		out, _ = json.Marshal(map[string]string{"error": rep.Error})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(append(out, '\n'))
}

func (s *Server) catalog(w http.ResponseWriter, r *http.Request, peer, machine string, fail func(int, string)) {
	p := filepath.Join(s.L.Published(peer), machine+".catalog")
	b, err := readRegular(p)
	if err != nil {
		fail(http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", LetterType+"; charset=utf-8")
	w.Header().Set(MachineHeader, machine)
	w.Header().Set("Cache-Control", "no-store")
	if fi, err := os.Stat(p); err == nil {
		w.Header().Set("Last-Modified", fi.ModTime().UTC().Format(http.TimeFormat))
	}
	_, _ = w.Write(b)
}

func (s *Server) store(w http.ResponseWriter, r *http.Request, dir string, max int64, placement bool, fail func(int, string)) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != LetterType || (params["charset"] != "" && !strings.EqualFold(params["charset"], "utf-8")) || len(params) > 1 ||
		(len(params) == 1 && params["charset"] == "") {
		fail(http.StatusUnsupportedMediaType, "a letter is "+LetterType+"; charset=utf-8")
		return
	}
	if r.ContentLength > max {
		fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("over %d bytes", max))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("over %d bytes", max))
			return
		}
		fail(http.StatusBadRequest, "the body can't be read")
		return
	}
	var id, ext string
	if placement {
		// The id names the file, so it comes from the letter itself: one
		// that doesn't decode is no placement. Who signed it is the
		// office's business, not the endpoint's.
		block, _, err := SplitLetter(body)
		var j *Job
		if err == nil {
			j, err = Decode(block)
		}
		if err != nil || !IDRE.MatchString(j.ID) {
			fail(http.StatusBadRequest, "not a placement")
			return
		}
		id, ext = j.ID, ".placement"
		if n := countSuffix(dir, ext); n >= MaxPending {
			fail(http.StatusServiceUnavailable, "the box is full")
			return
		}
	} else {
		id, ext = r.URL.Query().Get("id"), ".reply"
		if !IDRE.MatchString(id) || len(r.URL.Query()) != 1 {
			fail(http.StatusBadRequest, "a reply needs ?id=<placement id>")
			return
		}
		if n := countSuffix(dir, ext); n >= MaxPending {
			fail(http.StatusServiceUnavailable, "the box is full")
			return
		}
	}
	if err := dropFile(dir, id+ext, body); err != nil {
		if errors.Is(err, ErrInBox) {
			fail(http.StatusConflict, "already in the box")
			return
		}
		fail(http.StatusInternalServerError, "can't store it")
		s.logf("storing %s%s: %v", id, ext, err)
		return
	}
	s.logf("stored %s%s in %s", id, ext, filepath.Base(dir))
	w.WriteHeader(http.StatusCreated)
}

// safeName is a box or an asker as one path segment: never a path.
func safeName(s string) bool { return boxName(s) == nil && IDRE.MatchString(s) }

func countSuffix(dir, suffix string) int {
	ents, _ := os.ReadDir(dir)
	n := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), suffix) {
			n++
		}
	}
	return n
}
