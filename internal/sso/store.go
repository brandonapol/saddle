package sso

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// DirEnv overrides where logins are cached.
const DirEnv = "SADDLE_SSO_DIR"

// Skew is how long before expiry a cached token counts as expired, so a
// token handed out is good for at least this long.
const Skew = time.Minute

// DefaultDir is $SADDLE_SSO_DIR, else saddle/sso under the user cache dir.
// It is per user, not per repo, so every repo and worktree using the same
// issuer and client shares one login.
func DefaultDir() (string, error) {
	if d := os.Getenv(DirEnv); d != "" {
		return d, nil
	}
	c, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(c, "saddle", "sso"), nil
}

// Store keeps logins on disk: one 0600 JSON file per issuer, client,
// scopes and audience, in a 0700 dir.
type Store struct {
	Dir    string
	Client Client
}

// key names a login's files. Two [sso] tables with the same issuer and
// client share a login.
func key(p Provider) string {
	scopes := p.Scopes
	if len(scopes) == 0 {
		scopes = DefaultScopes
	}
	h := sha256.Sum256([]byte(strings.Join([]string{strings.TrimRight(p.Issuer, "/"), p.ClientID, strings.Join(scopes, " "), p.Audience}, "\n")))
	return hex.EncodeToString(h[:8])
}

func (s Store) path(p Provider, ext string) string {
	return filepath.Join(s.Dir, key(p)+ext)
}

func (s Store) ensureDir() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.Dir, 0o700)
}

// lock takes the login's exclusive lock, so concurrent callers refresh
// once: a provider that rotates refresh tokens would revoke the family on
// a reused one.
func (s Store) lock(p Provider) (func(), error) {
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path(p, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// Load reads the cached tokens; ok is false when there are none.
func (s Store) Load(p Provider) (Tokens, bool, error) {
	var t Tokens
	ok, err := s.readJSON(s.path(p, ".json"), &t)
	return t, ok, err
}

// Save writes tokens atomically with mode 0600.
func (s Store) Save(p Provider, t Tokens) error {
	return s.writeJSON(s.path(p, ".json"), t)
}

// Logout deletes the cached login and any pending sign-in.
func (s Store) Logout(p Provider) error {
	var errs []error
	for _, ext := range []string{".json", ".pending.json"} {
		if err := os.Remove(s.path(p, ext)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s Store) readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

func (s Store) writeJSON(path string, v any) error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// usable reports whether t can still yield a token without a new login.
func usable(t Tokens, kind string, now time.Time) bool {
	if t.Pick(kind) != "" && now.Add(Skew).Before(t.ExpiresAt) {
		return true
	}
	return t.RefreshToken != "" && (t.RefreshExpiresAt.IsZero() || now.Before(t.RefreshExpiresAt))
}

// Status is a login's state, without its tokens.
type Status struct {
	LoggedIn  bool
	ExpiresAt time.Time // the token's expiry
	// Refreshable is whether a refresh token can renew it.
	Refreshable bool
	// Pending is a sign-in started and not finished, if any.
	Pending *DeviceAuth
}

// Status reads p's login state from disk, with no network.
func (s Store) Status(p Provider) (Status, error) {
	t, ok, err := s.Load(p)
	if err != nil {
		return Status{}, err
	}
	now := s.Client.now()
	st := Status{}
	if ok {
		st.LoggedIn = usable(t, p.Token, now)
		st.ExpiresAt = t.ExpiresAt
		st.Refreshable = t.RefreshToken != "" && (t.RefreshExpiresAt.IsZero() || now.Before(t.RefreshExpiresAt))
	}
	var da DeviceAuth
	if ok, err := s.readJSON(s.path(p, ".pending.json"), &da); err == nil && ok && now.Before(da.ExpiresAt) {
		st.Pending = &da
	}
	return st, nil
}

// Token returns a token for p good for at least Skew: the cached one, else
// a refreshed one. ErrLoginRequired (wrapped) means only a new sign-in helps.
func (s Store) Token(ctx context.Context, p Provider) (string, error) {
	unlock, err := s.lock(p)
	if err != nil {
		return "", err
	}
	defer unlock()
	t, ok, err := s.Load(p)
	if err != nil {
		return "", err
	}
	now := s.Client.now()
	if ok {
		if tok := t.Pick(p.Token); tok != "" && now.Add(Skew).Before(t.ExpiresAt) {
			return tok, nil
		}
	}
	if !ok || t.RefreshToken == "" || (!t.RefreshExpiresAt.IsZero() && !now.Before(t.RefreshExpiresAt)) {
		return "", fmt.Errorf("sso %s: %w", p.Name, ErrLoginRequired)
	}
	nt, err := s.Client.Refresh(ctx, p, t.RefreshToken)
	if IsInvalidGrant(err) {
		// The refresh token is dead; keep nothing that can't be used.
		_ = os.Remove(s.path(p, ".json"))
		return "", fmt.Errorf("%v: %w", err, ErrLoginRequired)
	}
	if err != nil {
		return "", err
	}
	if err := s.Save(p, nt); err != nil {
		return "", err
	}
	tok := nt.Pick(p.Token)
	if tok == "" {
		return "", fmt.Errorf("sso %s: the provider returned no %s token; check [sso.%s] token and scopes", p.Name, p.Token, p.Name)
	}
	return tok, nil
}

// Login runs the device flow in the foreground: show is called once with
// the sign-in instructions, then it waits for approval and saves the tokens.
func (s Store) Login(ctx context.Context, p Provider, show func(DeviceAuth)) error {
	da, err := s.Client.StartDevice(ctx, p)
	if err != nil {
		return err
	}
	show(da)
	return s.finish(ctx, p, da)
}

func (s Store) finish(ctx context.Context, p Provider, da DeviceAuth) error {
	t, err := s.Client.PollDevice(ctx, p, da)
	if err != nil {
		return err
	}
	unlock, err := s.lock(p)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.Save(p, t); err != nil {
		return err
	}
	_ = os.Remove(s.path(p, ".pending.json"))
	return nil
}

// StartPending returns the sign-in a background login waits on, starting
// one when none is pending. fresh is true when this call started it, so
// only one caller tells the owner and starts the poller.
func (s Store) StartPending(ctx context.Context, p Provider) (da DeviceAuth, fresh bool, err error) {
	unlock, err := s.lock(p)
	if err != nil {
		return da, false, err
	}
	defer unlock()
	path := s.path(p, ".pending.json")
	if ok, err := s.readJSON(path, &da); err == nil && ok && s.Client.now().Before(da.ExpiresAt) {
		return da, false, nil
	}
	da, err = s.Client.StartDevice(ctx, p)
	if err != nil {
		return da, false, err
	}
	if err := s.writeJSON(path, da); err != nil {
		return da, false, err
	}
	return da, true, nil
}

// FinishPending waits for the pending sign-in's approval and saves the
// tokens. The pending file is removed either way, so a later failure starts
// a new sign-in.
func (s Store) FinishPending(ctx context.Context, p Provider) error {
	path := s.path(p, ".pending.json")
	var da DeviceAuth
	ok, err := s.readJSON(path, &da)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("sso %s: no sign-in is pending", p.Name)
	}
	err = s.finish(ctx, p, da)
	if err != nil {
		_ = os.Remove(path)
	}
	return err
}
