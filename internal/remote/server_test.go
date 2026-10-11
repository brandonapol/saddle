package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeSource struct{}

func (fakeSource) Snapshot() (Snapshot, error) {
	return Snapshot{Integration: "saddle/integration", Counts: Counts{Running: 2, NeedsYou: 1},
		Tasks: []TaskLine{{ID: "t3", Title: "meter", Status: "needs_you"}}}, nil
}

func (fakeSource) NeedsYou() ([]NeedsYouItem, error) {
	return []NeedsYouItem{{Task: "t3", Title: "meter", Kind: KindPrompt, Text: "Claude needs your permission to use Bash"}}, nil
}

type rig struct {
	srv    *httptest.Server
	tokens *Tokens
	dir    string
	events []AuditEntry
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newRigOpts(t, Options{})
}

func newRigOpts(t *testing.T, opts Options) *rig {
	t.Helper()
	if opts.Limits == (Limits{}) {
		opts.Limits = Limits{PerMinute: 1000, FailsPerMinute: 1000}
	}
	r := &rig{dir: t.TempDir()}
	r.tokens = NewTokens(r.dir)
	audit, err := OpenAudit(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	audit.Mirror = func(e AuditEntry) { r.events = append(r.events, e) }
	r.srv = httptest.NewServer(Handler(fakeSource{}, r.tokens, audit, opts))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) token(t *testing.T, name string, scopes ...Scope) string {
	t.Helper()
	s, err := r.tokens.Create(name, scopes, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type bearer struct {
	secret string
	extra  http.Header
}

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if b.secret != "" {
		req.Header.Set("Authorization", "Bearer "+b.secret)
	}
	for k, v := range b.extra {
		req.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (r *rig) connect(t *testing.T, secret string) (*mcp.ClientSession, error) {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: r.srv.URL + MCPPath, HTTPClient: &http.Client{Transport: bearer{secret: secret}},
		MaxRetries: -1, DisableStandaloneSSE: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "remote-test"}, nil).Connect(ctx, tr, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func (r *rig) post(t *testing.T, secret string, h http.Header) *http.Response {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req, err := http.NewRequest(http.MethodPost, r.srv.URL+MCPPath, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	for k, v := range h {
		req.Header[k] = v
	}
	if host := h.Get("Host"); host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestNoTokenIsUnauthorized(t *testing.T) {
	r := newRig(t)
	if resp := r.post(t, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", resp.StatusCode)
	}
	if resp := r.post(t, SecretPrefix+"bogus", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token: status %d, want 401", resp.StatusCode)
	}
	if len(r.events) != 2 || r.events[0].Decision != DecisionDenied || r.events[0].Reason == "" {
		t.Fatalf("auth failures not audited: %+v", r.events)
	}
}

// TestReadTokenCannotLand is the spike's acceptance test (#251): a read
// token sees status and needs-you, and land is refused and audited.
func TestReadTokenCannotLand(t *testing.T) {
	r := newRig(t)
	cs, err := r.connect(t, r.token(t, "phone", ScopeRead))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range lt.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"needs_you", "status"}) {
		t.Fatalf("read token sees tools %v, want [needs_you status]", names)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "status"})
	if err != nil || res.IsError {
		t.Fatalf("status: %v %+v", err, res)
	}
	var snap Snapshot
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &snap); err != nil || snap.Counts.NeedsYou != 1 {
		t.Fatalf("status snapshot = %+v (%v)", snap, err)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "needs_you"})
	if err != nil || res.IsError {
		t.Fatalf("needs_you: %v %+v", err, res)
	}
	var ny NeedsYouOut
	b, _ = json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &ny); err != nil || len(ny.Items) != 1 || ny.Items[0].Task != "t3" {
		t.Fatalf("needs_you = %+v (%v)", ny, err)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "land"})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("land with a read token succeeded: %+v", res)
	}

	var got []string
	for _, e := range r.events {
		got = append(got, e.Tool+":"+e.Decision)
		if e.Token != "phone" {
			t.Errorf("audit entry without the token name: %+v", e)
		}
	}
	want := []string{"status:" + DecisionAllowed, "needs_you:" + DecisionAllowed, "land:" + DecisionDenied}
	if !slices.Equal(got, want) {
		t.Fatalf("audit = %v, want %v", got, want)
	}
}

// TestActTokenStillCannotRunUnimplementedActions: the spike exposes no action
// tools at all, so even a token scoped for them reaches nothing that changes
// state.
func TestActTokenStillCannotRunUnimplementedActions(t *testing.T) {
	r := newRig(t)
	cs, err := r.connect(t, r.token(t, "laptop", ScopeRead, ScopeAct, ScopeLand))
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "land"})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("land succeeded in the read-only spike: %+v", res)
	}
}

func TestRevokedTokenStopsAtOnce(t *testing.T) {
	r := newRig(t)
	secret := r.token(t, "phone", ScopeRead)
	if resp := r.post(t, secret, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("live token: status %d, want 200", resp.StatusCode)
	}
	if err := NewTokens(r.dir).Revoke("phone"); err != nil {
		t.Fatal(err)
	}
	if resp := r.post(t, secret, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token: status %d, want 401", resp.StatusCode)
	}
}

// TestBrowserRequestsAreRefused guards against a web page driving the
// listener from the owner's browser: requests with an Origin header, or for
// a Host that isn't loopback (DNS rebinding), are refused before auth.
func TestBrowserRequestsAreRefused(t *testing.T) {
	r := newRig(t)
	secret := r.token(t, "phone", ScopeRead)
	if resp := r.post(t, secret, http.Header{"Origin": {"https://evil.example"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Origin header: status %d, want 403", resp.StatusCode)
	}
	if resp := r.post(t, secret, http.Header{"Host": {"evil.example:7431"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host: status %d, want 403", resp.StatusCode)
	}
}

func TestOtherPathsAreNotFound(t *testing.T) {
	r := newRig(t)
	secret := r.token(t, "phone", ScopeRead)
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/v1/land", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /v1/land: status %d, want 404", resp.StatusCode)
	}
}

func TestFailedAuthIsRateLimited(t *testing.T) {
	dir := t.TempDir()
	audit, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = audit.Close() }()
	srv := httptest.NewServer(Handler(fakeSource{}, NewTokens(dir), audit, Options{Limits: Limits{PerMinute: 100, FailsPerMinute: 2}}))
	defer srv.Close()
	r := &rig{srv: srv}
	var codes []int
	for range 4 {
		codes = append(codes, r.post(t, "wrong", nil).StatusCode)
	}
	if !slices.Equal(codes, []int{401, 401, 429, 429}) {
		t.Fatalf("codes = %v, want two 401s then 429s", codes)
	}
}

func TestTokenRequestsAreRateLimited(t *testing.T) {
	dir := t.TempDir()
	tokens := NewTokens(dir)
	audit, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = audit.Close() }()
	srv := httptest.NewServer(Handler(fakeSource{}, tokens, audit, Options{Limits: Limits{PerMinute: 2, FailsPerMinute: 100}}))
	defer srv.Close()
	r := &rig{srv: srv, tokens: tokens}
	secret := r.token(t, "chatty", ScopeRead)
	var codes []int
	for range 3 {
		codes = append(codes, r.post(t, secret, nil).StatusCode)
	}
	if !slices.Equal(codes, []int{200, 200, 429}) {
		t.Fatalf("codes = %v, want 200 200 429", codes)
	}
}

func TestAuditLogIsPrivateJSONL(t *testing.T) {
	r := newRig(t)
	r.post(t, "", nil)
	p := filepath.Join(r.dir, auditFile)
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("audit log mode %v, want 0600", fi.Mode().Perm())
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		var e AuditEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("audit line %q: %v", sc.Text(), err)
		}
		if e.Decision != DecisionDenied || e.Addr == "" || e.TS.IsZero() {
			t.Fatalf("audit entry = %+v", e)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("audit has %d lines, want 1", n)
	}
}
