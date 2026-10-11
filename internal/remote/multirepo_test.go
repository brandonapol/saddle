package remote

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// repoSource is a read-only Source that says which repo it is.
type repoSource struct {
	fakeSource
	name string
}

func (s repoSource) Snapshot() (Snapshot, error) {
	return Snapshot{Integration: s.name + "/integration"}, nil
}

type multiRig struct {
	rig
	trusted map[string]bool
}

func newMultiRig(t *testing.T, repos ...ServedRepo) *multiRig {
	t.Helper()
	m := &multiRig{rig: rig{dir: t.TempDir()}, trusted: map[string]bool{}}
	for _, r := range repos {
		m.trusted[r.Root] = true
	}
	m.tokens = NewTokens(m.dir)
	audit, err := OpenAudit(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	audit.Mirror = func(e AuditEntry) { m.events = append(m.events, e) }
	opts := Options{Limits: Limits{PerMinute: 1000, FailsPerMinute: 1000}, Trusted: func(root string) error {
		if !m.trusted[root] {
			return errors.New("not trusted")
		}
		return nil
	}}
	m.srv = httptest.NewServer(ReposHandler(repos, m.tokens, audit, opts))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *multiRig) connectPath(t *testing.T, path, secret string) (*mcp.ClientSession, error) {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: m.srv.URL + path, HTTPClient: &http.Client{Transport: bearer{secret: secret}},
		MaxRetries: -1, DisableStandaloneSSE: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "remote-test"}, nil).Connect(ctx, tr, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func (m *multiRig) status(t *testing.T, path, secret string) (int, string) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{}}}`
	req, _ := http.NewRequest(http.MethodPost, m.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

func twoRepos() []ServedRepo {
	return []ServedRepo{
		{Name: "saddle", Root: "/src/saddle", Src: repoSource{name: "saddle"}},
		{Name: "quark", Root: "/src/quark", Src: repoSource{name: "quark"}},
	}
}

// TestReposAreRoutedByName: one listener serves /mcp/<repo> for each
// repo, and bare /mcp is the first one listed.
func TestReposAreRoutedByName(t *testing.T) {
	m := newMultiRig(t, twoRepos()...)
	secret := m.token(t, "phone", ScopeRead)
	for path, want := range map[string]string{"/mcp/saddle": "saddle/integration", "/mcp/quark": "quark/integration", "/mcp": "saddle/integration"} {
		code, body := m.status(t, path, secret)
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s: %d %s, want %s", path, code, body, want)
		}
	}
	for _, path := range []string{"/mcp/nope", "/mcp/saddle/x", "/mcp/"} {
		if code, _ := m.status(t, path, secret); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, code)
		}
	}
	for _, e := range m.events {
		if e.Tool == "status" && e.Repo == "" {
			t.Fatalf("audit entry without its repo: %+v", e)
		}
	}
}

// TestRepoAllowlistPerRoute: a token limited to quark reaches /mcp/quark
// and is refused, and audited, on /mcp/saddle.
func TestRepoAllowlistPerRoute(t *testing.T) {
	m := newMultiRig(t, twoRepos()...)
	quark, err := m.tokens.Create("quark-only", []Scope{ScopeRead}, time.Hour, "quark")
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := m.status(t, "/mcp/quark", quark); code != http.StatusOK {
		t.Fatalf("/mcp/quark: %d", code)
	}
	if code, _ := m.status(t, "/mcp/saddle", quark); code != http.StatusForbidden {
		t.Fatalf("/mcp/saddle with a quark token: %d, want 403", code)
	}
	last := m.events[len(m.events)-1]
	if last.Decision != DecisionDenied || last.Repo != "saddle" || last.Token != "quark-only" {
		t.Fatalf("refusal audited as %+v", last)
	}
}

// TestUntrustedRepoIsRefused: trust is checked on every request, so a repo
// whose trust was revoked stops being served at once.
func TestUntrustedRepoIsRefused(t *testing.T) {
	m := newMultiRig(t, twoRepos()...)
	secret := m.token(t, "phone", ScopeRead)
	if code, _ := m.status(t, "/mcp/quark", secret); code != http.StatusOK {
		t.Fatalf("trusted: %d", code)
	}
	m.trusted["/src/quark"] = false
	if code, _ := m.status(t, "/mcp/quark", secret); code != http.StatusForbidden {
		t.Fatalf("untrusted: %d, want 403", code)
	}
	if code, _ := m.status(t, "/mcp/saddle", secret); code != http.StatusOK {
		t.Fatalf("the other repo stopped too: %d", code)
	}
	last := m.events[len(m.events)-1]
	found := false
	for _, e := range m.events {
		if e.Repo == "quark" && e.Decision == DecisionDenied && strings.Contains(e.Reason, "trust") {
			found = true
		}
	}
	if !found {
		t.Fatalf("untrusted refusal not audited: %+v", last)
	}
}

// TestConfirmationIsBoundToRepo: a code held on one repo's route can't be
// confirmed on another's.
func TestConfirmationIsBoundToRepo(t *testing.T) {
	a, b := newFakeActor(), newFakeActor()
	m := newMultiRig(t, ServedRepo{Name: "saddle", Root: "/src/saddle", Src: a}, ServedRepo{Name: "quark", Root: "/src/quark", Src: b})
	secret := m.token(t, "owner", ScopeAdmin)
	sa, err := m.connectPath(t, "/mcp/saddle", secret)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := m.connectPath(t, "/mcp/quark", secret)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := sa.CallTool(ctx, &mcp.CallToolParams{Name: "land", Arguments: map[string]any{"if_version": a.version("queue")}})
	if callErr(res, err) {
		t.Fatal(err)
	}
	code := confirmCode(t, res)
	res, err = sb.CallTool(ctx, &mcp.CallToolParams{Name: ConfirmTool, Arguments: map[string]any{"code": code}})
	if !callErr(res, err) || len(a.got())+len(b.got()) != 0 {
		t.Fatalf("a saddle code was confirmed on quark: %v %v", a.got(), b.got())
	}
	if _, err := sa.CallTool(ctx, &mcp.CallToolParams{Name: ConfirmTool, Arguments: map[string]any{"code": code}}); err != nil || len(a.got()) != 1 {
		t.Fatalf("confirm on its own repo: %v %v", err, a.got())
	}
}

func TestConfigRepos(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	writeFile(t, p, "[remote]\nenabled = true\nrepos = [\"/src/saddle\", \"/src/quark/\"]\n")
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := cfg.RepoList()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0] != "/src/saddle" || rs[1] != "/src/quark" {
		t.Fatalf("repos = %v", rs)
	}
	for _, bad := range []string{`["src/saddle"]`, `["/a/saddle", "/b/saddle"]`, `["/a/bad name"]`} {
		writeFile(t, p, "[remote]\nrepos = "+bad+"\n")
		cfg, err := LoadConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cfg.RepoList(); err == nil {
			t.Errorf("repos = %s accepted", bad)
		}
		if err := cfg.Check(); err == nil {
			t.Errorf("Check accepted repos = %s", bad)
		}
	}
}

// TestListedRepoCheck: with [remote] repos set, only listed repos are
// served (stdio too).
func TestListedRepoCheck(t *testing.T) {
	cfg := Config{Repos: []string{"/src/saddle"}}
	if err := cfg.CheckListed("/src/saddle"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.CheckListed("/src/quark"); err == nil {
		t.Fatal("an unlisted repo was accepted")
	}
	if err := (Config{}).CheckListed("/src/quark"); err != nil {
		t.Fatalf("with no list every trusted repo is served: %v", err)
	}
}
