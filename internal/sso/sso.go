// Package sso logs saddle in to an OIDC identity provider (Keycloak, Okta,
// or any provider with OIDC discovery and the device-authorization grant)
// once, caches the tokens, and hands child agents a fresh bearer token on
// demand (#758). Codex runs `saddle sso token` as a model provider's auth
// command, so no token ever sits in a launch file, an env var or a brief.
package sso

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Provider is one [sso.<name>] table: an OIDC issuer and a public client.
type Provider struct {
	Name     string
	Issuer   string
	ClientID string
	Scopes   []string
	// Audience is sent as audience= on the device and refresh requests, for
	// Okta custom authorization servers and Auth0. Empty sends none.
	Audience string
	// Token is which token the provider's consumers get: "access" (default)
	// or "id".
	Token string
}

// DefaultScopes asks for an ID token and a refresh token.
var DefaultScopes = []string{"openid", "offline_access"}

// Token kinds.
const (
	TokenAccess = "access"
	TokenID     = "id"
)

// ErrLoginRequired means there is no usable login: run the device flow.
var ErrLoginRequired = errors.New("sso login required")

// CheckIssuer refuses an issuer that isn't an absolute https URL. Plain http
// is allowed only for loopback hosts (local development and tests).
func CheckIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return fmt.Errorf("issuer %q: want an absolute URL", issuer)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("issuer %q: want https", issuer)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Client talks to an OIDC provider.
type Client struct {
	HTTP *http.Client
	Now  func() time.Time
	// Sleep waits between device-flow polls; tests make it instant.
	Sleep func(context.Context, time.Duration) error
}

func (c Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Endpoints are the parts of OIDC discovery saddle uses.
type Endpoints struct {
	Token  string `json:"token_endpoint"`
	Device string `json:"device_authorization_endpoint"`
}

// Discover reads the issuer's /.well-known/openid-configuration.
func (c Client) Discover(ctx context.Context, p Provider) (Endpoints, error) {
	var ep Endpoints
	if err := CheckIssuer(p.Issuer); err != nil {
		return ep, err
	}
	u := strings.TrimRight(p.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ep, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return ep, fmt.Errorf("sso %s: discovery: %w", p.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ep, fmt.Errorf("sso %s: discovery: %s", p.Name, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ep); err != nil {
		return ep, fmt.Errorf("sso %s: discovery: %w", p.Name, err)
	}
	if ep.Token == "" {
		return ep, fmt.Errorf("sso %s: discovery has no token_endpoint", p.Name)
	}
	return ep, nil
}

// DeviceAuth is a pending device-authorization request (RFC 8628). Only
// VerificationURI(Complete) and UserCode are shown to the owner; DeviceCode
// stays in the 0600 pending file.
type DeviceAuth struct {
	DeviceCode              string    `json:"device_code"`
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete,omitempty"`
	Interval                int       `json:"interval,omitempty"`
	ExpiresAt               time.Time `json:"expires_at"`
}

// URL is where the owner signs in: the complete URI when the provider gives
// one, so no code needs typing.
func (d DeviceAuth) URL() string {
	if d.VerificationURIComplete != "" {
		return d.VerificationURIComplete
	}
	return d.VerificationURI
}

// Prompt is the one-line sign-in instruction for the owner.
func (d DeviceAuth) Prompt() string {
	return fmt.Sprintf("open %s and enter code %s", d.URL(), d.UserCode)
}

func (c Client) scopes(p Provider) string {
	s := p.Scopes
	if len(s) == 0 {
		s = DefaultScopes
	}
	return strings.Join(s, " ")
}

// StartDevice asks for a device code.
func (c Client) StartDevice(ctx context.Context, p Provider) (DeviceAuth, error) {
	var da DeviceAuth
	ep, err := c.Discover(ctx, p)
	if err != nil {
		return da, err
	}
	if ep.Device == "" {
		return da, fmt.Errorf("sso %s: the issuer has no device_authorization_endpoint; enable the device authorization grant for client %s", p.Name, p.ClientID)
	}
	form := url.Values{"client_id": {p.ClientID}, "scope": {c.scopes(p)}}
	if p.Audience != "" {
		form.Set("audience", p.Audience)
	}
	var raw struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURL         string `json:"verification_url"` // some providers' older spelling
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := c.post(ctx, p, ep.Device, form, &raw); err != nil {
		return da, err
	}
	if raw.VerificationURI == "" {
		raw.VerificationURI = raw.VerificationURL
	}
	if raw.DeviceCode == "" || raw.UserCode == "" || raw.VerificationURI == "" {
		return da, fmt.Errorf("sso %s: device authorization response is missing fields", p.Name)
	}
	if raw.ExpiresIn <= 0 {
		raw.ExpiresIn = 600
	}
	return DeviceAuth{
		DeviceCode: raw.DeviceCode, UserCode: raw.UserCode,
		VerificationURI: raw.VerificationURI, VerificationURIComplete: raw.VerificationURIComplete,
		Interval: raw.Interval, ExpiresAt: c.now().Add(time.Duration(raw.ExpiresIn) * time.Second),
	}, nil
}

// PollDevice polls the token endpoint until the owner approves da, it is
// denied, or it expires.
func (c Client) PollDevice(ctx context.Context, p Provider, da DeviceAuth) (Tokens, error) {
	ep, err := c.Discover(ctx, p)
	if err != nil {
		return Tokens{}, err
	}
	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {da.DeviceCode},
		"client_id":   {p.ClientID},
	}
	for {
		if !da.ExpiresAt.IsZero() && !c.now().Before(da.ExpiresAt) {
			return Tokens{}, fmt.Errorf("sso %s: the sign-in code expired; run saddle sso login %s again", p.Name, p.Name)
		}
		if err := c.sleep(ctx, interval); err != nil {
			return Tokens{}, err
		}
		t, err := c.tokenRequest(ctx, p, ep.Token, form)
		var oe *oauthError
		if errors.As(err, &oe) {
			switch oe.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			case "access_denied":
				return Tokens{}, fmt.Errorf("sso %s: sign-in was denied", p.Name)
			case "expired_token":
				return Tokens{}, fmt.Errorf("sso %s: the sign-in code expired; run saddle sso login %s again", p.Name, p.Name)
			}
		}
		return t, err
	}
}

// Refresh trades a refresh token for new tokens. A provider that doesn't
// rotate refresh tokens leaves the old one in place.
func (c Client) Refresh(ctx context.Context, p Provider, refresh string) (Tokens, error) {
	ep, err := c.Discover(ctx, p)
	if err != nil {
		return Tokens{}, err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {p.ClientID},
	}
	if p.Audience != "" {
		form.Set("audience", p.Audience)
	}
	t, err := c.tokenRequest(ctx, p, ep.Token, form)
	if err != nil {
		return t, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refresh
	}
	return t, nil
}

// Tokens are a login's cached tokens.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	IDToken      string    `json:"id_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	// RefreshExpiresAt is when the refresh token lapses, when the provider
	// says (Keycloak's refresh_expires_in). Zero is unknown.
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitzero"`
}

// Pick is the token a consumer gets for kind ("access" or "id").
func (t Tokens) Pick(kind string) string {
	if kind == TokenID {
		return t.IDToken
	}
	return t.AccessToken
}

// tokenResponse is a token endpoint's success body.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	IDToken          string `json:"id_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
}

// oauthError is an RFC 6749 error body. It never carries a token.
type oauthError struct {
	Provider    string
	Code        string `json:"error"`
	Description string `json:"error_description"`
	Status      string
}

func (e *oauthError) Error() string {
	s := fmt.Sprintf("sso %s: %s", e.Provider, e.Code)
	if e.Description != "" {
		s += ": " + e.Description
	}
	return s
}

// IsInvalidGrant reports whether err is the provider refusing a refresh
// token (expired, revoked or reused): only a new login helps.
func IsInvalidGrant(err error) bool {
	var oe *oauthError
	return errors.As(err, &oe) && (oe.Code == "invalid_grant" || oe.Code == "invalid_token" || oe.Code == "unauthorized_client")
}

func (c Client) tokenRequest(ctx context.Context, p Provider, endpoint string, form url.Values) (Tokens, error) {
	var tr tokenResponse
	if err := c.post(ctx, p, endpoint, form, &tr); err != nil {
		return Tokens{}, err
	}
	if tr.AccessToken == "" {
		return Tokens{}, fmt.Errorf("sso %s: token response has no access_token", p.Name)
	}
	now := c.now()
	t := Tokens{AccessToken: tr.AccessToken, IDToken: tr.IDToken, RefreshToken: tr.RefreshToken}
	switch {
	case tr.ExpiresIn > 0:
		t.ExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	default:
		// No expires_in: read the JWT's exp, unverified, only to know when
		// to refresh. A token that isn't a JWT gets five minutes.
		if exp, ok := jwtExp(tr.AccessToken); ok {
			t.ExpiresAt = exp
		} else {
			t.ExpiresAt = now.Add(5 * time.Minute)
		}
	}
	if tr.RefreshExpiresIn > 0 {
		t.RefreshExpiresAt = now.Add(time.Duration(tr.RefreshExpiresIn) * time.Second)
	}
	return t, nil
}

// post sends form to endpoint and decodes a 2xx JSON body into out. Error
// bodies are reduced to their error code and description, so a response
// can't leak into a log.
func (c Client) post(ctx context.Context, p Provider, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("sso %s: %w", p.Name, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("sso %s: %w", p.Name, err)
	}
	if resp.StatusCode/100 != 2 {
		oe := &oauthError{Provider: p.Name, Status: resp.Status}
		if json.Unmarshal(body, oe) != nil || oe.Code == "" {
			oe.Code = "http " + strconv.Itoa(resp.StatusCode)
			oe.Description = ""
		}
		return oe
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("sso %s: bad response from %s: %v", p.Name, endpoint, err)
	}
	return nil
}

// redactURLError drops the request body from transport errors; url.Error
// carries only the method and URL, which hold no secret here.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %v", ue.Op, ue.URL, ue.Err)
	}
	return err
}

// jwtExp reads a JWT's exp claim without verifying it.
func jwtExp(tok string) (time.Time, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
