package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTokenRepoAllowlist(t *testing.T) {
	cases := []struct {
		repos []string
		root  string
		want  bool
	}{
		{nil, "/src/saddle", true}, // no allowlist: any repo
		{[]string{"saddle"}, "/src/saddle", true},
		{[]string{"/src/saddle"}, "/src/saddle", true},
		{[]string{"/src/saddle/"}, "/src/saddle", true},
		{[]string{"quark"}, "/src/saddle", false},
		{[]string{"/other/saddle"}, "/src/saddle", false},
		{[]string{"quark", "saddle"}, "/src/saddle", true},
		{[]string{"saddle"}, "", false}, // a server that names no repo can't satisfy an allowlist
	}
	for _, c := range cases {
		if got := (Token{Repos: c.repos}).AllowsRepo(c.root); got != c.want {
			t.Errorf("Token{Repos: %v}.AllowsRepo(%q) = %v, want %v", c.repos, c.root, got, c.want)
		}
	}
}

func TestTokenCreateStoresRepos(t *testing.T) {
	ts := NewTokens(t.TempDir())
	secret, err := ts.Create("ci", []Scope{ScopeRead}, time.Hour, "saddle", "/src/quark/")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ts.Verify(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tok.Repos, []string{"saddle", "/src/quark"}) {
		t.Fatalf("repos = %v", tok.Repos)
	}
	if _, err := ts.Create("bad", []Scope{ScopeRead}, time.Hour, "  "); err == nil {
		t.Fatal("Create accepted a blank repo")
	}
	if _, err := ts.Create("rel", []Scope{ScopeRead}, time.Hour, "src/quark"); err == nil {
		t.Fatal("Create accepted a relative repo path")
	}
}

// TestTokenForOtherRepoIsRefused: a token limited to quark can't read
// saddle's server, and the refusal is audited.
func TestTokenForOtherRepoIsRefused(t *testing.T) {
	r := newRigOpts(t, Options{Repo: "/src/saddle"})
	quark, err := r.tokens.Create("quark-only", []Scope{ScopeRead}, time.Hour, "quark")
	if err != nil {
		t.Fatal(err)
	}
	if resp := r.post(t, quark, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("token for another repo: status %d, want 403", resp.StatusCode)
	}
	if len(r.events) != 1 || r.events[0].Token != "quark-only" || r.events[0].Decision != DecisionDenied ||
		!strings.Contains(r.events[0].Reason, "saddle") {
		t.Fatalf("audit = %+v", r.events)
	}
	ok, err := r.tokens.Create("saddle-only", []Scope{ScopeRead}, time.Hour, "saddle")
	if err != nil {
		t.Fatal(err)
	}
	if resp := r.post(t, ok, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("token for this repo: status %d, want 200", resp.StatusCode)
	}
}

// fakeKill is an admin tool served only in tests, so the confirmation round
// trip can be exercised before any real admin tool is served remotely.
type fakeKill struct {
	mu    sync.Mutex
	calls []string
}

type killIn struct {
	Task string `json:"task"`
}

func (f *fakeKill) def() toolDef {
	return toolDef{name: "kill", add: func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: "kill", Description: "test kill"},
			func(_ context.Context, _ *mcp.CallToolRequest, in killIn) (*mcp.CallToolResult, struct{}, error) {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.calls = append(f.calls, in.Task)
				return nil, struct{}{}, nil
			})
	}}
}

func (f *fakeKill) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func callErr(res *mcp.CallToolResult, err error) bool {
	return err != nil || (res != nil && res.IsError)
}

func confirmCode(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var p ConfirmRequired
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &p); err != nil || p.Code == "" {
		t.Fatalf("no confirmation code in %s (%v)", b, err)
	}
	return p.Code
}

func auditTrail(r *rig) []string {
	var out []string
	for _, e := range r.events {
		out = append(out, e.Tool+":"+e.Decision)
	}
	return out
}

// TestAdminToolNeedsConfirmation is the round trip: the first call runs
// nothing and returns a code; confirm with that code runs the call exactly
// as first sent, once.
func TestAdminToolNeedsConfirmation(t *testing.T) {
	k := &fakeKill{}
	r := newRigOpts(t, Options{extra: []toolDef{k.def()}})
	cs, err := r.connect(t, r.token(t, "owner", ScopeAdmin))
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
	if !slices.Contains(names, "confirm") || !slices.Contains(names, "kill") {
		t.Fatalf("admin token sees %v, want kill and confirm", names)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "kill", Arguments: map[string]any{"task": "t7"}})
	if callErr(res, err) {
		t.Fatalf("kill: %v %+v", err, res)
	}
	if len(k.got()) != 0 {
		t.Fatal("kill ran before it was confirmed")
	}
	if !strings.Contains(text(res), "confirm") || !strings.Contains(text(res), "t7") {
		t.Fatalf("kill result doesn't explain the confirmation: %q", text(res))
	}
	code := confirmCode(t, res)

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "confirm", Arguments: map[string]any{"code": code}})
	if callErr(res, err) {
		t.Fatalf("confirm: %v %+v", err, res)
	}
	if got := k.got(); !slices.Equal(got, []string{"t7"}) {
		t.Fatalf("kill ran with %v, want [t7]", got)
	}

	// A code works once.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "confirm", Arguments: map[string]any{"code": code}})
	if !callErr(res, err) {
		t.Fatal("a confirmation code was accepted twice")
	}
	if len(k.got()) != 1 {
		t.Fatalf("kill ran %d times", len(k.got()))
	}
	want := []string{"kill:" + DecisionPending, "kill:" + DecisionAllowed, "confirm:" + DecisionDenied}
	if got := auditTrail(r); !slices.Equal(got, want) {
		t.Fatalf("audit = %v, want %v", got, want)
	}
}

// TestConfirmationIsBoundToToken: another token, even an admin one, can't
// confirm a call it didn't make.
func TestConfirmationIsBoundToToken(t *testing.T) {
	k := &fakeKill{}
	r := newRigOpts(t, Options{extra: []toolDef{k.def()}})
	a, err := r.connect(t, r.token(t, "a", ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.connect(t, r.token(t, "b", ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := a.CallTool(ctx, &mcp.CallToolParams{Name: "kill", Arguments: map[string]any{"task": "t1"}})
	if callErr(res, err) {
		t.Fatal(err)
	}
	code := confirmCode(t, res)
	res, err = b.CallTool(ctx, &mcp.CallToolParams{Name: "confirm", Arguments: map[string]any{"code": code}})
	if !callErr(res, err) || len(k.got()) != 0 {
		t.Fatalf("token b confirmed token a's kill: %+v %v", res, k.got())
	}
	// It is still a's to confirm.
	res, err = a.CallTool(ctx, &mcp.CallToolParams{Name: "confirm", Arguments: map[string]any{"code": code}})
	if callErr(res, err) || len(k.got()) != 1 {
		t.Fatalf("a's confirm after b's attempt: %v %+v %v", err, res, k.got())
	}
}

func TestConfirmationExpires(t *testing.T) {
	k := &fakeKill{}
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	r := newRigOpts(t, Options{extra: []toolDef{k.def()}, now: clock})
	cs, err := r.connect(t, r.token(t, "owner", ScopeAdmin))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "kill", Arguments: map[string]any{"task": "t1"}})
	if callErr(res, err) {
		t.Fatal(err)
	}
	code := confirmCode(t, res)
	mu.Lock()
	now = now.Add(ConfirmTTL + time.Second)
	mu.Unlock()
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "confirm", Arguments: map[string]any{"code": code}})
	if !callErr(res, err) || len(k.got()) != 0 {
		t.Fatalf("an expired confirmation ran kill: %+v %v", res, k.got())
	}
}

// TestConfirmNeedsAdmin: confirm is itself an admin tool, hidden from and
// refused to lesser tokens.
func TestConfirmNeedsAdmin(t *testing.T) {
	r := newRigOpts(t, Options{extra: []toolDef{(&fakeKill{}).def()}})
	cs, err := r.connect(t, r.token(t, "laptop", ScopeAct, ScopeLand))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range lt.Tools {
		if tl.Name == "confirm" || tl.Name == "kill" {
			t.Fatalf("non-admin token sees %q", tl.Name)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "confirm", Arguments: map[string]any{"code": "x"}})
	if !callErr(res, err) {
		t.Fatal("non-admin confirm succeeded")
	}
}
