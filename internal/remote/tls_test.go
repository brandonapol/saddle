package remote

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
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

// selfSigned writes a cert and key for 127.0.0.1 and box.example into dir
// and returns their paths and a pool that trusts the cert.
func selfSigned(t *testing.T, dir string) (cert, key string, pool *x509.CertPool) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "saddle-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"box.example"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeFile(t, cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, key, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})))
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(c)
	return cert, key, pool
}

// TestCheckPublicNeedsAllowPublicAndTLS: a bind that isn't loopback needs
// both an explicit allow_public and TLS. Either alone is refused.
func TestCheckPublicNeedsAllowPublicAndTLS(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := selfSigned(t, dir)
	cases := []struct {
		name string
		cfg  Config
		want string // "" means ok; else a substring of the error
	}{
		{"loopback", Config{Listen: "127.0.0.1:7431"}, ""},
		{"loopback with tls", Config{Listen: "127.0.0.1:7431", TLSCert: cert, TLSKey: key}, ""},
		{"public, nothing", Config{Listen: "0.0.0.0:7431"}, "allow_public"},
		{"public, tls only", Config{Listen: "0.0.0.0:7431", TLSCert: cert, TLSKey: key}, "allow_public"},
		{"public, allow only", Config{Listen: "0.0.0.0:7431", AllowPublic: true}, "tls"},
		{"public, cert without key", Config{Listen: "0.0.0.0:7431", AllowPublic: true, TLSCert: cert}, "tls_key"},
		{"public, missing cert", Config{Listen: "0.0.0.0:7431", AllowPublic: true, TLSCert: filepath.Join(dir, "nope.pem"), TLSKey: key}, "nope.pem"},
		{"public, allowed", Config{Listen: "0.0.0.0:7431", AllowPublic: true, TLSCert: cert, TLSKey: key}, ""},
		{"no port", Config{Listen: "127.0.0.1"}, "127.0.0.1"},
	}
	for _, c := range cases {
		err := c.cfg.Check()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: Check = %v, want ok", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: Check = %v, want an error about %q", c.name, err, c.want)
		}
	}
}

// TestCheckRefusesLooseTLSKey: like the tokens file, a private key others
// can read is refused.
func TestCheckRefusesLooseTLSKey(t *testing.T) {
	cert, key, _ := selfSigned(t, t.TempDir())
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	err := Config{Listen: "0.0.0.0:7431", AllowPublic: true, TLSCert: cert, TLSKey: key}.Check()
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("Check with a 0644 key = %v, want refused", err)
	}
}

func TestConfigReadsTLSAndHosts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, p, "[remote]\nenabled = true\nlisten = \"100.64.0.5:7431\"\nallow_public = true\n"+
		"tls_cert = \"/c.pem\"\ntls_key = \"/k.pem\"\nhosts = [\"box.example\"]\n")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.AllowPublic || c.TLSCert != "/c.pem" || c.TLSKey != "/k.pem" || len(c.Hosts) != 1 || c.Hosts[0] != "box.example" {
		t.Fatalf("config = %+v", c)
	}
}

// TestListenServesTLS: with a cert configured, the listener speaks only TLS.
func TestListenServesTLS(t *testing.T) {
	cert, key, pool := selfSigned(t, t.TempDir())
	cfg := Config{Listen: "127.0.0.1:0", TLSCert: cert, TLSKey: key}
	ln, err := cfg.listen()
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }),
		ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	addr := ln.Addr().String()

	if resp, err := http.Get("http://" + addr + "/"); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("plain http was served on a TLS listener")
		}
	}
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	resp, err := c.Get("https://" + addr + "/")
	if err != nil {
		t.Fatalf("https: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("https: status %d, tls %+v", resp.StatusCode, resp.TLS)
	}
}

// TestPublicHostsAreAllowlisted: a public listener accepts a Host it was
// told about (its own name behind TLS) and still refuses any other, which
// keeps DNS rebinding out.
func TestPublicHostsAreAllowlisted(t *testing.T) {
	r := newRigOpts(t, Options{Hosts: []string{"box.example"}})
	secret := r.token(t, "phone", ScopeRead)
	if resp := r.post(t, secret, http.Header{"Host": {"box.example:7431"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("listed Host: status %d, want 200", resp.StatusCode)
	}
	if resp := r.post(t, secret, http.Header{"Host": {"evil.example:7431"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unlisted Host: status %d, want 403", resp.StatusCode)
	}
	if resp := r.post(t, secret, http.Header{"Host": {"box.example"}, "Origin": {"https://box.example"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("browser on a listed Host: status %d, want 403", resp.StatusCode)
	}
}
