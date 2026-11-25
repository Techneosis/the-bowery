package offices

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// endpointLayout is a dev-mode layout with one office, "reviews", on this
// machine, and its box "office-reviews".
func endpointLayout(t *testing.T) Layout {
	t.Helper()
	root := t.TempDir()
	l := Layout{Dev: true, Etc: filepath.Join(root, "etc"), Var: filepath.Join(root, "var")}
	if err := os.MkdirAll(l.Etc, 0o755); err != nil {
		t.Fatal(err)
	}
	here := ThisMachine()
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(l.RegionFile(), fmt.Sprintf("region = \"region-a\"\nowner = \"alice\"\ntimezone = \"UTC\"\n"+
		"offices = [{ name = \"reviews\", machine = %q }, { name = \"faraway\", machine = \"box-9\" }]\n", here))
	write(l.MachineFile(), fmt.Sprintf("machine = %q\ntransport = \"http\"\n", here))
	return l
}

func placementBody(id string) []byte {
	return []byte("```placement\nasked_by = \"bob\"\nexpires = \"2099-01-01T00:00:00Z\"\nid = \"" + id +
		"\"\nkind = \"review\"\n```\n")
}

func post(t *testing.T, url, ctype string, body []byte) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

const textType = "text/plain; charset=utf-8"

func TestTheEndpointStoresAPlacementInTheBox(t *testing.T) {
	l := endpointLayout(t)
	srv := httptest.NewServer(&Server{L: l})
	defer srv.Close()
	if r := post(t, srv.URL+"/placements/office-reviews", textType, placementBody("p-1")); r.StatusCode != http.StatusCreated {
		t.Fatalf("POST: %s", r.Status)
	}
	got, err := os.ReadFile(filepath.Join(l.BoxDir("office-reviews"), "p-1.placement"))
	if err != nil || !bytes.Equal(got, placementBody("p-1")) {
		t.Fatalf("not stored as sent: %q %v", got, err)
	}
	// The same id again: already in the box.
	if r := post(t, srv.URL+"/placements/office-reviews", textType, placementBody("p-1")); r.StatusCode != http.StatusConflict {
		t.Fatalf("a second copy: %s", r.Status)
	}
	// Not a placement at all: nothing is stored.
	if r := post(t, srv.URL+"/placements/office-reviews", textType, []byte("hello\n")); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("junk: %s", r.Status)
	}
	// A reply, by id, into an asker's box that exists.
	os.MkdirAll(l.BoxDir("asker-bob"), 0o755)
	if r := post(t, srv.URL+"/replies/bob?id=p-1", textType, []byte("refused p-1\n")); r.StatusCode != http.StatusCreated {
		t.Fatalf("reply: %s", r.Status)
	}
	if b, _ := os.ReadFile(filepath.Join(l.BoxDir("asker-bob"), "p-1.reply")); string(b) != "refused p-1\n" {
		t.Fatalf("reply stored as %q", b)
	}
	if r := post(t, srv.URL+"/replies/bob", textType, []byte("x\n")); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("a reply with no id: %s", r.Status)
	}
}

func TestTheEndpointRefusesAnOversizedBody(t *testing.T) {
	l := endpointLayout(t)
	srv := httptest.NewServer(&Server{L: l})
	defer srv.Close()
	big := append(placementBody("p-big"), bytes.Repeat([]byte("x"), MaxPlacement)...)
	if r := post(t, srv.URL+"/placements/office-reviews", textType, big); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized placement: %s", r.Status)
	}
	// Chunked, so no Content-Length gives it away: still capped while read.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/placements/office-reviews", io.MultiReader(bytes.NewReader(big)))
	req.Header.Set("Content-Type", textType)
	req.ContentLength = -1
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized chunked placement: %s", resp.Status)
	}
	os.MkdirAll(l.BoxDir("asker-bob"), 0o755)
	if r := post(t, srv.URL+"/replies/bob?id=p-1", textType, bytes.Repeat([]byte("x"), MaxReply+1)); r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized reply: %s", r.Status)
	}
	ents, _ := os.ReadDir(l.BoxDir("office-reviews"))
	ents2, _ := os.ReadDir(l.BoxDir("asker-bob"))
	if len(ents)+len(ents2) != 0 {
		t.Fatalf("something was stored: %v %v", ents, ents2)
	}
}

func TestTheEndpointRefusesTraversalAndAnythingElse(t *testing.T) {
	l := endpointLayout(t)
	// Something to reach for, if a path could get out.
	if err := os.WriteFile(filepath.Join(l.Var, "secret"), []byte("s"), 0o644); err != nil {
		os.MkdirAll(l.Var, 0o755)
		os.WriteFile(filepath.Join(l.Var, "secret"), []byte("s"), 0o644)
	}
	srv := httptest.NewServer(&Server{L: l})
	defer srv.Close()
	for _, p := range []string{
		"/placements/..", "/placements/%2E%2E", "/placements/..%2Fsecret", "/placements/office-reviews%2F..%2F..",
		"/placements/office-reviews/x", "/placements/", "/placements/office-faraway", "/placements/office-nobody",
		"/replies/..", "/replies/%2E%2E%2Fboxes", "/replies/a%2Fb", "/replies/bob/x",
		"/", "/boxes/office-reviews", "/secret",
	} {
		r := post(t, srv.URL+p+"?id=p-1", textType, placementBody("p-1"))
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: %s", p, r.Status)
		}
	}
	for _, p := range []string{"/catalog/..", "/catalog/%2E%2E", "/catalog/..%2Fsecret", "/catalog/region-b/x", "/catalog/", "/secret", "/"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %s", p, resp.Status)
		}
	}
	// Nothing landed outside a box, and no box was made for a bad name.
	var stored []string
	filepath.Walk(l.Var, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && p != filepath.Join(l.Var, "secret") {
			stored = append(stored, p)
		}
		return nil
	})
	if len(stored) != 0 {
		t.Fatalf("stored: %v", stored)
	}
	// The wrong method, and the wrong content type.
	resp, err := http.Get(srv.URL + "/placements/office-reviews")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET a box: %s", resp.Status)
	}
	if r := post(t, srv.URL+"/catalog/region-b", textType, nil); r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST a catalog: %s", r.Status)
	}
	for _, ct := range []string{"", "application/json", "text/plain; charset=latin1", "text/html", "text/plain; charset=utf-8; x=y"} {
		if r := post(t, srv.URL+"/placements/office-reviews", ct, placementBody("p-ct")); r.StatusCode != http.StatusUnsupportedMediaType {
			t.Errorf("content type %q: %s", ct, r.Status)
		}
	}
	if r := post(t, srv.URL+"/placements/office-reviews", "text/plain", placementBody("p-ok")); r.StatusCode != http.StatusCreated {
		t.Fatalf("plain text/plain: %s", r.Status)
	}
}

func TestTheClientCarriesPlacementsRepliesAndCatalogs(t *testing.T) {
	a, b := endpointLayout(t), endpointLayout(t)
	sa := httptest.NewServer(&Server{L: a})
	defer sa.Close()
	sb := httptest.NewServer(&Server{L: b})
	defer sb.Close()
	tb := NewHTTPTransport(b, 0) // region-b's side, sending to a
	if err := tb.Send(sa.URL, "office-reviews", "p-2", placementBody("p-2")); err != nil {
		t.Fatal(err)
	}
	ls, err := NewHTTPTransport(a, 0).Inbox("office-reviews").List()
	if err != nil || len(ls) != 1 || ls[0].ID != "p-2" {
		t.Fatalf("a's inbox: %+v %v", ls, err)
	}
	// A 4xx is not retried and is an error.
	if err := tb.Send(sa.URL, "office-nobody", "p-3", placementBody("p-3")); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("send to no such box: %v", err)
	}
	ta := NewHTTPTransport(a, 0)
	os.MkdirAll(b.BoxDir("asker-bob"), 0o755)
	if err := ta.Reply(sb.URL, "bob", "p-2", []byte("refused p-2\n")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(b.BoxDir("asker-bob"), "p-2.reply")); string(got) != "refused p-2\n" {
		t.Fatalf("reply: %q", got)
	}
	// Nothing published for region-b yet: no catalogs, no error.
	if rs, err := tb.Catalogs("region-a", sa.URL, "region-b"); err != nil || len(rs) != 0 {
		t.Fatalf("before publish: %v %v", rs, err)
	}
	if err := ta.Publish("region-b", sb.URL, "region-a", ThisMachine(), []byte("catalog body\n")); err != nil {
		t.Fatal(err)
	}
	rs, err := tb.Catalogs("region-a", sa.URL, "region-b")
	if err != nil || len(rs) != 1 || string(rs[0].Body) != "catalog body\n" || rs[0].Machine != ThisMachine() {
		t.Fatalf("catalogs: %+v %v", rs, err)
	}
	// Another peer's catalog isn't region-b's to have.
	if rs, _ := NewHTTPTransport(b, 0).Catalogs("region-a", sa.URL, "region-c"); len(rs) != 0 {
		t.Fatalf("region-c got region-b's catalog: %+v", rs)
	}
	// An address that isn't a URL is refused before anything is sent.
	for _, bad := range []string{"/var/lib/offices", "ftp://x", "http://", "http://u:p@h", "http://h/?q=1", "http://h/#f"} {
		if err := tb.Send(bad, "office-reviews", "p-4", placementBody("p-4")); err == nil {
			t.Errorf("address %q was used", bad)
		}
	}
}

func TestTheClientRetriesAServerError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			http.Error(w, "busy", http.StatusServiceUnavailable)
		case 2:
			// The first try's letter, stored before its answer was lost.
			http.Error(w, "already in the box", http.StatusConflict)
		}
	}))
	defer srv.Close()
	tr := NewHTTPTransport(endpointLayout(t), 0)
	tr.Backoff = time.Millisecond
	if err := tr.Send(srv.URL, "office-reviews", "p-5", placementBody("p-5")); err != nil || calls != 2 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
}

// The plan's latency rule: a POST wakes the office's long-poll at once, not
// at a poll or the backstop.
func TestAPostWakesThePullLoopWithin50ms(t *testing.T) {
	l := endpointLayout(t)
	srv := httptest.NewServer(&Server{L: l})
	defer srv.Close()
	in := NewHTTPTransport(l, time.Hour).Inbox("office-reviews") // a poll would never come
	if ok, err := in.Wait(context.Background(), 10*time.Millisecond); ok || err != nil {
		t.Fatalf("an empty box woke: %v %v", ok, err)
	}
	for i := 0; i < 5; i++ {
		woke := make(chan time.Time, 1)
		go func() {
			ok, err := in.Wait(context.Background(), 10*time.Second)
			if !ok || err != nil {
				t.Errorf("wait: %v %v", ok, err)
			}
			woke <- time.Now()
		}()
		time.Sleep(50 * time.Millisecond) // the waiter is blocked on its watch
		sent := time.Now()
		if r := post(t, srv.URL+"/placements/office-reviews", textType, placementBody(fmt.Sprintf("w-%d", i))); r.StatusCode != http.StatusCreated {
			t.Fatalf("POST: %s", r.Status)
		}
		select {
		case at := <-woke:
			if d := at.Sub(sent); d > 50*time.Millisecond {
				t.Fatalf("woke %v after the POST", d)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the POST didn't wake the loop")
		}
	}
}

// Two copies of one id arriving together: exactly one is stored, and the
// other is told 409. A stat followed by a rename lets the later body replace
// the earlier one.
func TestConcurrentPostsOfOneIdStoreOneAndRefuseTheRest(t *testing.T) {
	l := endpointLayout(t)
	srv := httptest.NewServer(&Server{L: l})
	defer srv.Close()
	const n = 40
	codes := make(chan int, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			body := bytes.Replace(placementBody("p-race"), []byte("bob"), []byte(fmt.Sprintf("bob%d", i)), 1)
			<-start
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/placements/office-reviews", bytes.NewReader(body))
			req.Header.Set("Content-Type", textType)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}(i)
	}
	close(start)
	created := 0
	for i := 0; i < n; i++ {
		switch c := <-codes; c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("status %d", c)
		}
	}
	if created != 1 {
		t.Fatalf("%d copies of one id were accepted", created)
	}
}

// A reply goes into an asker's box that is already there. A stranger can't
// make the machine grow a directory per name.
func TestARepliesDoesNotMakeAnAskerBox(t *testing.T) {
	l := endpointLayout(t)
	srv := httptest.NewServer(&Server{L: l})
	defer srv.Close()
	if r := post(t, srv.URL+"/replies/nobody?id=p-1", textType, []byte("hello\n")); r.StatusCode != http.StatusNotFound {
		t.Fatalf("a reply to an unknown asker: %s", r.Status)
	}
	if _, err := os.Stat(l.BoxDir("asker-nobody")); err == nil {
		t.Fatal("the endpoint made a box for a name it was never given a letter from")
	}
	if err := os.MkdirAll(l.BoxDir("asker-bob"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := post(t, srv.URL+"/replies/bob?id=p-1", textType, []byte("hello\n")); r.StatusCode != http.StatusCreated {
		t.Fatalf("a reply to a known asker: %s", r.Status)
	}
}
