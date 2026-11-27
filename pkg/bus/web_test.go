package bus

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	webHost   = "node.example.ts.net"
	webOwner  = "alice@example.com"
	webOrigin = "https://board.example.ts.net"
)

// webNode is a node whose seat alice reads and publishes private.*, and
// whose web handler believes whatever who says about a caller.
func webNode(t *testing.T, who func(string) (WhoIs, error)) (*Node, http.Handler) {
	t.Helper()
	k := key(t)
	n := peerNode(t, "north", k, nil, Rules{Seats: map[string]Topics{
		"alice": {Publish: Patterns{"private.*", "job.*"}, Read: Patterns{"private.*", "job.*"}},
	}})
	h, err := n.WebHandler(WebConfig{
		Host: webHost, Owner: webOwner, Origin: webOrigin, Seat: "alice",
		WhoIs: func(_ context.Context, addr string) (WhoIs, error) { return who(addr) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return n, h
}

func owner(string) (WhoIs, error) { return WhoIs{Login: webOwner}, nil }

func webReq(method, path, origin, body string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "https://"+webHost+path, rd)
	r.RemoteAddr = "100.64.0.7:51234"
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type snapReply struct {
	Snapshot PrivateState `json:"snapshot"`
	CSRF     string       `json:"csrf"`
}

func snap(t *testing.T, h http.Handler) snapReply {
	t.Helper()
	w := do(h, webReq("GET", "/private/snapshot", webOrigin, ""))
	if w.Code != 200 {
		t.Fatalf("snapshot: %d %s", w.Code, w.Body)
	}
	var s snapReply
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Only the owner's own devices are served: another login, a tagged node
// (a peer's node, a server) even when its owner is the owner, and a caller
// whois can't place are all refused, before anything is read.
func TestWebWrongWhoisIs403(t *testing.T) {
	cases := map[string]func(string) (WhoIs, error){
		"another login": func(string) (WhoIs, error) { return WhoIs{Login: "bob@example.com"}, nil },
		"a tagged node": func(string) (WhoIs, error) { return WhoIs{Login: webOwner, Tagged: true}, nil },
		"no login":      func(string) (WhoIs, error) { return WhoIs{}, nil },
		"whois fails":   func(string) (WhoIs, error) { return WhoIs{}, errors.New("no such peer") },
		"case differs":  func(string) (WhoIs, error) { return WhoIs{Login: "Alice@example.com"}, nil },
	}
	for name, who := range cases {
		n, h := webNode(t, who)
		if _, err := n.Publish("alice", Publication{Kind: "private.card", Data: json.RawMessage(`{"key":"x","subject":"s"}`)}); err != nil {
			t.Fatal(err)
		}
		for _, r := range []*http.Request{
			webReq("GET", "/private/snapshot", webOrigin, ""),
			webReq("GET", "/private/stream", webOrigin, ""),
			webReq("POST", "/private/publish", webOrigin, `{"kind":"private.card","data":{"subject":"s"}}`),
		} {
			w := do(h, r)
			if w.Code != 403 {
				t.Errorf("%s: %s %s: %d, want 403", name, r.Method, r.URL.Path, w.Code)
			}
			if strings.Contains(w.Body.String(), "private/x") {
				t.Errorf("%s: %s leaked a record: %s", name, r.URL.Path, w.Body)
			}
		}
	}
}

// Whois answers are kept a little while for the owner only: a caller
// refused once is asked about again, and is served once it's the owner.
func TestWebWhoisKeepsOnlyTheOwner(t *testing.T) {
	login, asks := "bob@example.com", 0
	_, h := webNode(t, func(string) (WhoIs, error) { asks++; return WhoIs{Login: login}, nil })
	if w := do(h, webReq("GET", "/private/snapshot", webOrigin, "")); w.Code != 403 {
		t.Fatalf("bob: %d", w.Code)
	}
	login = webOwner
	for i := 0; i < 3; i++ {
		if w := do(h, webReq("GET", "/private/snapshot", webOrigin, "")); w.Code != 200 {
			t.Fatalf("the owner, after a refusal: %d", w.Code)
		}
	}
	if asks != 2 {
		t.Fatalf("%d whois calls, want 2 (the refusal, then the owner once)", asks)
	}
}

// A page on any other origin is refused, even on a GET (the browser would
// hide the answer from it anyway; the node doesn't give one).
func TestWebWrongOriginIs403(t *testing.T) {
	_, h := webNode(t, owner)
	for _, o := range []string{"https://evil.example", "https://board.example.ts.net.evil.example", "http://board.example.ts.net", "null"} {
		for _, r := range []*http.Request{
			webReq("GET", "/private/snapshot", o, ""),
			webReq("POST", "/private/publish", o, `{"kind":"private.card","data":{"subject":"s"}}`),
			webReq("OPTIONS", "/private/publish", o, ""),
		} {
			if w := do(h, r); w.Code != 403 {
				t.Errorf("Origin %s: %s %s: %d, want 403", o, r.Method, r.URL.Path, w.Code)
			} else if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Errorf("Origin %s: answered CORS", o)
			}
		}
	}
	// A write must say where it's from: no Origin is no write.
	s := snap(t, h)
	r := webReq("POST", "/private/publish", "", `{"kind":"private.card","data":{"subject":"s"}}`)
	r.Header.Set("X-CSRF-Token", s.CSRF)
	if w := do(h, r); w.Code != 403 {
		t.Errorf("POST with no Origin: %d, want 403", w.Code)
	}
	// Another Host (a rebound name) is refused too.
	r = webReq("GET", "/private/snapshot", webOrigin, "")
	r.Host = "evil.example"
	if w := do(h, r); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("Host evil.example: %d, want 421", w.Code)
	}
}

// A write carries the node's CSRF token, minted for this caller; none, a
// made-up one or an expired one is refused and writes nothing.
func TestWebMissingCSRFIs403(t *testing.T) {
	n, h := webNode(t, owner)
	clock := time.Now()
	h2, err := n.WebHandler(WebConfig{Host: webHost, Owner: webOwner, Origin: webOrigin, Seat: "alice",
		WhoIs:  func(context.Context, string) (WhoIs, error) { return WhoIs{Login: webOwner}, nil },
		Secret: []byte("k"), Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"kind":"private.card","data":{"key":"watching","subject":"Things I'm watching"}}`
	old := snap(t, h2).CSRF
	clock = clock.Add(13 * time.Hour)
	for name, tok := range map[string]string{"none": "", "made up": "1.AAAA", "another node's": snap(t, h).CSRF, "expired": old} {
		r := webReq("POST", "/private/publish", webOrigin, body)
		if tok != "" {
			r.Header.Set("X-CSRF-Token", tok)
		}
		if w := do(h2, r); w.Code != 403 {
			t.Errorf("%s token: %d, want 403", name, w.Code)
		}
	}
	if got := snap(t, h2).Snapshot.Lists["card"]; len(got) != 0 {
		t.Fatalf("a refused write wrote: %v", got)
	}
	// With the token it lands, as the seat, and the snapshot has it.
	s := snap(t, h2)
	r := webReq("POST", "/private/publish", webOrigin, body)
	r.Header.Set("X-CSRF-Token", s.CSRF)
	w := do(h2, r)
	if w.Code != 200 {
		t.Fatalf("POST with the token: %d %s", w.Code, w.Body)
	}
	var rep struct {
		Ref    string `json:"ref"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil || rep.Ref != "north/alice:1" || rep.Cursor == "" {
		t.Fatalf("reply %s (%v)", w.Body, err)
	}
	if _, _, ok := snap(t, h2).Snapshot.Find("private/watching"); !ok {
		t.Fatal("the card isn't in the snapshot")
	}
}

// Only private records go in through the browser, each checked against
// its topic's schema.
func TestWebPublishesPrivateOnly(t *testing.T) {
	_, h := webNode(t, owner)
	tok := snap(t, h).CSRF
	for body, want := range map[string]int{
		`{"kind":"job.started","data":{}}`:                     400,
		`{"kind":"private.card","data":{"subject":1}}`:         400,
		`{"kind":"private.card","data":{"nope":"x"}}`:          400,
		`{"kind":"private.card"}`:                              400,
		`not json`:                                             400,
		`{"kind":"private.note","data":{"on":"k","text":"t"}}`: 200,
	} {
		r := webReq("POST", "/private/publish", webOrigin, body)
		r.Header.Set("X-CSRF-Token", tok)
		if w := do(h, r); w.Code != want {
			t.Errorf("%s: %d, want %d (%s)", body, w.Code, want, w.Body)
		}
	}
}

// CORS names the board's origin, never "*", and only to that origin.
func TestWebCORSNamesOneOrigin(t *testing.T) {
	_, h := webNode(t, owner)
	r := webReq("OPTIONS", "/private/publish", webOrigin, "")
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "content-type, x-csrf-token")
	w := do(h, r)
	if w.Code != 204 {
		t.Fatalf("preflight: %d", w.Code)
	}
	if got := w.Header().Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != webOrigin {
		t.Fatalf("Allow-Origin %q, want only %s", got, webOrigin)
	}
	hdrs := strings.ToLower(w.Header().Get("Access-Control-Allow-Headers"))
	if !strings.Contains(hdrs, "x-csrf-token") || !strings.Contains(hdrs, "content-type") {
		t.Fatalf("Allow-Headers %q", hdrs)
	}
	if w.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("credentials allowed: whois is the identity, nothing else")
	}
	w = do(h, webReq("GET", "/private/snapshot", webOrigin, ""))
	if got := w.Header().Values("Access-Control-Allow-Origin"); len(got) != 1 || got[0] != webOrigin {
		t.Fatalf("GET Allow-Origin %q", got)
	}
	if !strings.Contains(w.Header().Get("Vary"), "Origin") {
		t.Fatal("no Vary: Origin")
	}
	// A same-origin or tool caller (no Origin) gets no CORS header at all.
	if w := do(h, webReq("GET", "/private/snapshot", "", "")); w.Code != 200 || w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("no Origin: %d, Allow-Origin %q", w.Code, w.Header().Get("Access-Control-Allow-Origin"))
	}
}

// The snapshot is the private projection, only what the seat reads, with
// the cursor it was read at; shared topics never show.
func TestWebSnapshot(t *testing.T) {
	n, h := webNode(t, owner)
	for _, p := range []Publication{
		{Kind: "private.card", Data: json.RawMessage(`{"key":"a","subject":"A"}`)},
		{Kind: "job.started", Subject: json.RawMessage(`{"job":"j-1"}`)},
		{Kind: "private.request", Data: json.RawMessage(`{"key":"r","title":"R","members":["key:shared"]}`)},
	} {
		if _, err := n.Publish("alice", p); err != nil {
			t.Fatal(err)
		}
	}
	s := snap(t, h)
	if len(s.Snapshot.Lists["card"]) != 1 || len(s.Snapshot.Lists["request"]) != 1 {
		t.Fatalf("lists %v", s.Snapshot.Lists)
	}
	if s.Snapshot.Cursor != "north/alice=3" {
		t.Fatalf("cursor %q, want north/alice=3 (read to the heads)", s.Snapshot.Cursor)
	}
	if s.CSRF == "" {
		t.Fatal("no CSRF token")
	}
	w := do(h, webReq("GET", "/private/snapshot", webOrigin, ""))
	if strings.Contains(w.Body.String(), "j-1") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("a shared record, or a cacheable answer: %s %v", w.Body, w.Header())
	}
}

type sse struct {
	event, id, data string
}

func readSSE(t *testing.T, rd *bufio.Reader) sse {
	t.Helper()
	var e sse
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "" && e.event != "":
			return e
		case strings.HasPrefix(line, "event: "):
			e.event = line[7:]
		case strings.HasPrefix(line, "id: "):
			e.id = line[4:]
		case strings.HasPrefix(line, "data: "):
			e.data += line[6:]
		}
	}
}

// The stream starts with the snapshot, then sends what changed as it
// changes, each with the cursor it brings the reader to; a reader whose
// cursor is current gets no snapshot again.
func TestWebStream(t *testing.T) {
	n, h := webNode(t, owner)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = webHost
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	if _, err := n.Publish("alice", Publication{Kind: "private.card", Data: json.RawMessage(`{"key":"a","subject":"A"}`)}); err != nil {
		t.Fatal(err)
	}
	open := func(cursor string) (*bufio.Reader, func()) {
		ctx, cancel := context.WithCancel(context.Background())
		u := srv.URL + "/private/stream"
		if cursor != "" {
			u += "?cursor=" + cursor
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		req.Header.Set("Origin", webOrigin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		return bufio.NewReader(resp.Body), func() { cancel(); resp.Body.Close() }
	}
	rd, done := open("")
	defer done()
	e := readSSE(t, rd)
	if e.event != "snapshot" || e.id != "north/alice=1" || !strings.Contains(e.data, `"private/a"`) {
		t.Fatalf("first event %+v", e)
	}
	start := time.Now()
	if _, err := n.Publish("alice", Publication{Kind: "job.started", Subject: json.RawMessage(`{"job":"j-1"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Publish("alice", Publication{Kind: "private.card", Data: json.RawMessage(`{"key":"b","subject":"B"}`)}); err != nil {
		t.Fatal(err)
	}
	e = readSSE(t, rd)
	t.Logf("a change reached the stream in %v", time.Since(start))
	var ch struct {
		Cursor string                    `json:"cursor"`
		Put    map[string][]PrivateEntry `json:"put"`
		Drop   map[string][]string       `json:"drop"`
	}
	if err := json.Unmarshal([]byte(e.data), &ch); err != nil {
		t.Fatal(err)
	}
	if e.event != "change" || e.id != "north/alice=3" || ch.Cursor != e.id || len(ch.Put["card"]) != 1 || ch.Put["card"][0].Key != "private/b" {
		t.Fatalf("change %+v", e)
	}
	if strings.Contains(e.data, "j-1") {
		t.Fatal("a shared record in the stream")
	}
	// A remove drops the key.
	if _, err := n.Publish("alice", Publication{Kind: "private.card", Data: json.RawMessage(`{"key":"a","remove":true}`)}); err != nil {
		t.Fatal(err)
	}
	e = readSSE(t, rd)
	if e.event != "change" || !strings.Contains(e.data, `"drop":{"card":["private/a"]}`) {
		t.Fatalf("remove %+v", e)
	}
	// Reconnecting at the current cursor: no snapshot, just ready.
	rd2, done2 := open("north/alice=4")
	defer done2()
	if e := readSSE(t, rd2); e.event != "ready" || e.id != "north/alice=4" {
		t.Fatalf("resume %+v", e)
	}
	// At an old one: the snapshot again.
	rd3, done3 := open("north/alice=1")
	defer done3()
	if e := readSSE(t, rd3); e.event != "snapshot" {
		t.Fatalf("stale resume %+v", e)
	}
}

func TestWebConfigNeedsEverything(t *testing.T) {
	n, _ := webNode(t, owner)
	who := func(context.Context, string) (WhoIs, error) { return WhoIs{}, nil }
	for name, c := range map[string]WebConfig{
		"no host":       {Owner: webOwner, Origin: webOrigin, Seat: "alice", WhoIs: who},
		"no owner":      {Host: webHost, Origin: webOrigin, Seat: "alice", WhoIs: who},
		"a star origin": {Host: webHost, Owner: webOwner, Origin: "*", Seat: "alice", WhoIs: who},
		"origin path":   {Host: webHost, Owner: webOwner, Origin: webOrigin + "/app", Seat: "alice", WhoIs: who},
		"http origin":   {Host: webHost, Owner: webOwner, Origin: "http://board.example.ts.net", Seat: "alice", WhoIs: who},
		"no seat":       {Host: webHost, Owner: webOwner, Origin: webOrigin, WhoIs: who},
		"no whois":      {Host: webHost, Owner: webOwner, Origin: webOrigin, Seat: "alice"},
	} {
		if _, err := n.WebHandler(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
