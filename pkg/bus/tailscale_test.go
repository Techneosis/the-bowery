package bus

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTailscaled answers the LocalAPI's whois and cert calls on a Unix
// socket, as tailscaled does.
func fakeTailscaled(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "ts.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func TestTailscaleWhoIs(t *testing.T) {
	var asked string
	sock := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/localapi/v0/whois" {
			http.NotFound(w, r)
			return
		}
		asked = r.URL.Query().Get("addr")
		switch {
		case strings.HasPrefix(asked, "100.64.0.7:"):
			w.Write([]byte(`{"Node":{"Name":"phone.example.ts.net.","Tags":null},"UserProfile":{"LoginName":"alice@example.com"}}`))
		case strings.HasPrefix(asked, "100.64.0.8:"):
			w.Write([]byte(`{"Node":{"Name":"srv.example.ts.net.","Tags":["tag:server"]},"UserProfile":{"LoginName":"tagged-devices"}}`))
		default:
			http.Error(w, "no match for IP:port", http.StatusNotFound)
		}
	})
	who := TailscaleWhoIs(sock)
	got, err := who(context.Background(), "100.64.0.7:51234")
	if err != nil || got != (WhoIs{Login: "alice@example.com"}) || asked != "100.64.0.7:51234" {
		t.Fatalf("owner's phone: %+v %v (asked %q)", got, err, asked)
	}
	if got, err := who(context.Background(), "100.64.0.8:1"); err != nil || !got.Tagged {
		t.Fatalf("tagged: %+v %v", got, err)
	}
	if _, err := who(context.Background(), "100.64.0.9:1"); err == nil {
		t.Fatal("an unknown caller read as someone")
	}
	if _, err := TailscaleWhoIs(filepath.Join(t.TempDir(), "none.sock"))(context.Background(), "100.64.0.7:1"); err == nil {
		t.Fatal("no tailscaled read as someone")
	}
}

func pairPEM(t *testing.T, host string, notAfter time.Time) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	return append(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
}

// The certificate comes from tailscaled for the node's name, is kept in a
// cache file (so a restart while tailscaled is down still serves), and is
// fetched again once it's near its end.
func TestTailscaleCert(t *testing.T) {
	calls := 0
	expiry := time.Now().Add(10 * 24 * time.Hour) // near its end: renewed
	sock := fakeTailscaled(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/localapi/v0/cert/node.example.ts.net" || r.URL.Query().Get("type") != "pair" {
			http.NotFound(w, r)
			return
		}
		calls++
		w.Write(pairPEM(t, "node.example.ts.net", expiry))
	})
	cache := filepath.Join(t.TempDir(), "web.pem")
	cs := NewTailscaleCert("node.example.ts.net", sock, cache, nil)
	if _, err := cs.GetCertificate(nil); err == nil {
		t.Fatal("a certificate before any fetch")
	}
	if err := cs.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err := cs.GetCertificate(nil)
	if err != nil || c.Leaf.DNSNames[0] != "node.example.ts.net" {
		t.Fatalf("%v %v", c, err)
	}
	if fi, err := os.Stat(cache); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache %v %v", fi, err)
	}
	// Near its end, Refresh fetches again; well within it, it doesn't.
	if err := cs.Refresh(context.Background()); err != nil || calls != 2 {
		t.Fatalf("renewal: %v, %d calls", err, calls)
	}
	expiry = time.Now().Add(80 * 24 * time.Hour)
	cs.Refresh(context.Background())
	if err := cs.Refresh(context.Background()); err != nil || calls != 3 {
		t.Fatalf("a fresh certificate fetched again: %v, %d calls", err, calls)
	}
	// A restart with tailscaled gone serves the cached one.
	again := NewTailscaleCert("node.example.ts.net", filepath.Join(t.TempDir(), "gone.sock"), cache, nil)
	if c, err := again.GetCertificate(nil); err != nil || c == nil {
		t.Fatalf("cached: %v", err)
	}
	if err := again.Refresh(context.Background()); err != nil {
		t.Fatalf("a fresh cached certificate needs no tailscaled: %v", err)
	}
}
