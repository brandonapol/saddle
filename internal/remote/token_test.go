package remote

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTokenCreateVerifyRevoke(t *testing.T) {
	dir := t.TempDir()
	ts := NewTokens(dir)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	ts.now = func() time.Time { return now }

	secret, err := ts.Create("phone", []Scope{ScopeRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, SecretPrefix) {
		t.Fatalf("secret %q lacks prefix %q", secret, SecretPrefix)
	}
	tok, err := ts.Verify(secret)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Name != "phone" || len(tok.Scopes) != 1 || tok.Scopes[0] != ScopeRead {
		t.Fatalf("Verify = %+v", tok)
	}

	// The secret itself is never written down, only its hash.
	b, err := os.ReadFile(filepath.Join(dir, tokensFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) || strings.Contains(string(b), strings.TrimPrefix(secret, SecretPrefix)) {
		t.Fatal("tokens file contains the plaintext secret")
	}

	if _, err := ts.Verify(secret + "x"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("Verify(wrong) = %v, want ErrBadToken", err)
	}
	if _, err := ts.Verify(""); !errors.Is(err, ErrBadToken) {
		t.Fatalf("Verify(empty) = %v, want ErrBadToken", err)
	}

	if err := ts.Revoke("phone"); err != nil {
		t.Fatal(err)
	}
	// Revocation takes effect at once, even through another handle on the
	// same file (a running server reads the file on every request).
	if _, err := NewTokens(dir).Verify(secret); !errors.Is(err, ErrBadToken) {
		t.Fatalf("Verify(revoked) = %v, want ErrBadToken", err)
	}
	if err := ts.Revoke("nope"); err == nil {
		t.Fatal("Revoke of an unknown token succeeded")
	}
}

func TestTokenExpires(t *testing.T) {
	ts := NewTokens(t.TempDir())
	now := time.Now()
	ts.now = func() time.Time { return now }
	secret, err := ts.Create("short", []Scope{ScopeRead}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := ts.Verify(secret); !errors.Is(err, ErrBadToken) {
		t.Fatalf("Verify(expired) = %v, want ErrBadToken", err)
	}
}

func TestTokenTTLBounds(t *testing.T) {
	ts := NewTokens(t.TempDir())
	if _, err := ts.Create("forever", []Scope{ScopeRead}, 0); err == nil {
		t.Fatal("Create accepted a token that never expires")
	}
	if _, err := ts.Create("long", []Scope{ScopeRead}, MaxTTL+time.Hour); err == nil {
		t.Fatal("Create accepted a TTL above MaxTTL")
	}
	if _, err := ts.Create("bad name", []Scope{ScopeRead}, time.Hour); err == nil {
		t.Fatal("Create accepted a name with a space")
	}
	if _, err := ts.Create("dup", []Scope{ScopeRead}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Create("dup", []Scope{ScopeRead}, time.Hour); err == nil {
		t.Fatal("Create accepted a duplicate live name")
	}
}

func TestTokenFilesArePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "remote")
	ts := NewTokens(dir)
	if _, err := ts.Create("a", []Scope{ScopeRead}, time.Hour); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, tokensFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("tokens file mode %v, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("token dir mode %v, want 0700", di.Mode().Perm())
	}
}

// TestTokenFileReadableByOthersIsRefused: like ssh with a loose key, a
// tokens file others can read is not trusted.
func TestTokenFileReadableByOthersIsRefused(t *testing.T) {
	dir := t.TempDir()
	ts := NewTokens(dir)
	secret, err := ts.Create("a", []Scope{ScopeRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, tokensFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Verify(secret); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("Verify with a 0644 tokens file = %v, want a chmod 600 error", err)
	}
}
