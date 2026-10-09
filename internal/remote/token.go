package remote

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const (
	// SecretPrefix starts every token secret, so a leaked one is easy to
	// grep for and to recognize.
	SecretPrefix = "saddle_rc_"
	// DefaultTTL is how long a token lives unless told otherwise.
	DefaultTTL = 24 * time.Hour
	// MaxTTL caps a token's life: tokens are short-lived by design.
	MaxTTL = 30 * 24 * time.Hour

	tokensFile = "tokens.json"
)

// ErrBadToken is any token that doesn't authenticate: unknown, revoked or
// expired. Callers don't learn which.
var ErrBadToken = errors.New("invalid, revoked or expired token")

var tokenName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Token is one issued token. Only the SHA-256 of its secret is kept.
type Token struct {
	Name    string     `json:"name"`
	Hash    string     `json:"hash"`
	Scopes  []Scope    `json:"scopes"`
	Created time.Time  `json:"created"`
	Expires time.Time  `json:"expires"`
	Revoked *time.Time `json:"revoked,omitempty"`
}

func (t Token) live(now time.Time) bool { return t.Revoked == nil && now.Before(t.Expires) }

// Tokens is the token file in a user-private directory. It is read on every
// Verify, so a revocation from another process takes effect at once.
type Tokens struct {
	dir string
	mu  sync.Mutex
	now func() time.Time
}

// NewTokens keeps tokens in dir (created 0700 on first write).
func NewTokens(dir string) *Tokens { return &Tokens{dir: dir, now: time.Now} }

// Dir is where the tokens live.
func (ts *Tokens) Dir() string { return ts.dir }

func (ts *Tokens) path() string { return filepath.Join(ts.dir, tokensFile) }

func (ts *Tokens) load() ([]Token, error) {
	p := ts.path()
	fi, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by others (mode %v); run chmod 600 on it", p, fi.Mode().Perm())
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var f struct {
		Tokens []Token `json:"tokens"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return f.Tokens, nil
}

// save writes the file 0600 through a temp file and a rename, so a reader
// never sees half of it.
func (ts *Tokens) save(toks []Token) error {
	if err := os.MkdirAll(ts.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(ts.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(struct {
		Tokens []Token `json:"tokens"`
	}{toks}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(ts.dir, ".tokens-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), ts.path())
}

func hashSecret(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Create issues a token and returns its secret, which is shown once and
// never stored.
func (ts *Tokens) Create(name string, scopes []Scope, ttl time.Duration) (string, error) {
	if !tokenName.MatchString(name) {
		return "", fmt.Errorf("token name %q: use letters, digits, '.', '_' or '-'", name)
	}
	if ttl <= 0 || ttl > MaxTTL {
		return "", fmt.Errorf("ttl %v: want more than 0 and at most %v", ttl, MaxTTL)
	}
	if len(scopes) == 0 {
		return "", errors.New("a token needs at least one scope")
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	toks, err := ts.load()
	if err != nil {
		return "", err
	}
	now := ts.now()
	for _, t := range toks {
		if t.Name == name && t.live(now) {
			return "", fmt.Errorf("a live token named %q exists; revoke it first", name)
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secret := SecretPrefix + base64.RawURLEncoding.EncodeToString(raw)
	toks = append(toks, Token{Name: name, Hash: hashSecret(secret), Scopes: scopes, Created: now, Expires: now.Add(ttl)})
	return secret, ts.save(toks)
}

// Verify returns the live token whose secret this is. It compares hashes in
// constant time and checks every token, so timing says nothing about which
// one, if any, nearly matched.
func (ts *Tokens) Verify(secret string) (Token, error) {
	ts.mu.Lock()
	toks, err := ts.load()
	ts.mu.Unlock()
	if err != nil {
		return Token{}, err
	}
	want, _ := hex.DecodeString(hashSecret(secret))
	var found *Token
	for i := range toks {
		have, err := hex.DecodeString(toks[i].Hash)
		if err != nil || len(have) != len(want) {
			continue
		}
		if subtle.ConstantTimeCompare(have, want) == 1 {
			found = &toks[i]
		}
	}
	if secret == "" || found == nil || !found.live(ts.now()) {
		return Token{}, ErrBadToken
	}
	return *found, nil
}

// Revoke marks every live token called name revoked.
func (ts *Tokens) Revoke(name string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	toks, err := ts.load()
	if err != nil {
		return err
	}
	now := ts.now()
	n := 0
	for i := range toks {
		if toks[i].Name == name && toks[i].Revoked == nil {
			toks[i].Revoked = &now
			n++
		}
	}
	if n == 0 {
		return fmt.Errorf("no token named %q", name)
	}
	return ts.save(toks)
}

// List returns every token, live or not.
func (ts *Tokens) List() ([]Token, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.load()
}
