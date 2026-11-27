package bus

// The tailnet's two answers the private web endpoint needs, from the local
// tailscaled's LocalAPI over its Unix socket: who is at a remote address
// (whois), and the node's own HTTPS certificate (tailscaled gets it from
// Let's Encrypt for the node's tailnet name, and caches it). Stdlib only.
//
// tailscaled hands a certificate to root, its operator (tailscale set
// --operator=<user>) or TS_PERMIT_CERT_UID; whois answers any local user.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// DefaultTailscaled is tailscaled's socket on Linux.
const DefaultTailscaled = "/run/tailscale/tailscaled.sock"

func localAPI(ctx context.Context, sock, path string) ([]byte, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://local-tailscaled.sock"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Tailscale-Cap", "100")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tailscaled at %s: %w", sock, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if len(b) > 200 {
			b = b[:200]
		}
		return nil, fmt.Errorf("tailscaled %s: %s: %s", path, resp.Status, b)
	}
	return b, nil
}

// TailscaleWhoIs asks tailscaled at sock who is at a remote address.
func TailscaleWhoIs(sock string) func(ctx context.Context, remoteAddr string) (WhoIs, error) {
	return func(ctx context.Context, remoteAddr string) (WhoIs, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		b, err := localAPI(ctx, sock, "/localapi/v0/whois?addr="+url.QueryEscape(remoteAddr))
		if err != nil {
			return WhoIs{}, err
		}
		var w struct {
			Node struct {
				Tags []string `json:"Tags"`
			} `json:"Node"`
			UserProfile struct {
				LoginName string `json:"LoginName"`
			} `json:"UserProfile"`
		}
		if err := json.Unmarshal(b, &w); err != nil {
			return WhoIs{}, fmt.Errorf("whois: %v", err)
		}
		if w.UserProfile.LoginName == "" {
			return WhoIs{}, errors.New("whois named no user")
		}
		return WhoIs{Login: w.UserProfile.LoginName, Tagged: len(w.Node.Tags) > 0}, nil
	}
}

// RenewWithin is how near its end a certificate is fetched again.
const RenewWithin = 30 * 24 * time.Hour

// TailscaleCert is the node's HTTPS certificate for its tailnet name, from
// tailscaled, with a copy in a cache file.
type TailscaleCert struct {
	host, sock, cache string
	logf              func(string, ...any)

	mu   sync.Mutex
	cert *tls.Certificate
}

// NewTailscaleCert starts from the cache file, if it holds a certificate
// for host.
func NewTailscaleCert(host, sock, cache string, logf func(string, ...any)) *TailscaleCert {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	cs := &TailscaleCert{host: host, sock: sock, cache: cache, logf: logf}
	if b, err := os.ReadFile(cache); err == nil {
		if c, err := parsePair(b); err == nil && c.Leaf.VerifyHostname(host) == nil {
			cs.cert = c
		}
	}
	return cs
}

// Refresh fetches the certificate if there's none or it's near its end.
func (cs *TailscaleCert) Refresh(ctx context.Context) error {
	cs.mu.Lock()
	have := cs.cert
	cs.mu.Unlock()
	if have != nil && time.Until(have.Leaf.NotAfter) > RenewWithin {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // a first issue goes to Let's Encrypt
	defer cancel()
	b, err := localAPI(ctx, cs.sock, "/localapi/v0/cert/"+url.PathEscape(cs.host)+"?type=pair")
	if err != nil {
		return err
	}
	c, err := parsePair(b)
	if err != nil {
		return err
	}
	if err := c.Leaf.VerifyHostname(cs.host); err != nil {
		return fmt.Errorf("tailscaled's certificate: %v", err)
	}
	cs.mu.Lock()
	cs.cert = c
	cs.mu.Unlock()
	cs.logf("web: certificate for %s, until %s", cs.host, c.Leaf.NotAfter.UTC().Format("2006-01-02"))
	if cs.cache != "" {
		if err := writeAtomic(cs.cache, b); err != nil {
			cs.logf("web: caching the certificate: %v", err)
		}
	}
	return nil
}

// Run keeps the certificate fresh: a look a day, every minute while there
// is none, hourly while a renewal fails.
func (cs *TailscaleCert) Run(ctx context.Context) {
	for {
		wait := 24 * time.Hour
		if err := cs.Refresh(ctx); err != nil {
			cs.logf("web: %v", err)
			cs.mu.Lock()
			none := cs.cert == nil
			cs.mu.Unlock()
			wait = time.Hour
			if none {
				wait = time.Minute
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// GetCertificate is for tls.Config.
func (cs *TailscaleCert) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.cert == nil {
		return nil, errors.New("no certificate yet")
	}
	return cs.cert, nil
}

// parsePair reads tailscaled's "pair": the key and the chain, PEM, in one
// body.
func parsePair(b []byte) (*tls.Certificate, error) {
	c, err := tls.X509KeyPair(b, b)
	if err != nil {
		return nil, fmt.Errorf("certificate: %w", err)
	}
	if c.Leaf == nil {
		if c.Leaf, err = x509.ParseCertificate(c.Certificate[0]); err != nil {
			return nil, err
		}
	}
	return &c, nil
}
