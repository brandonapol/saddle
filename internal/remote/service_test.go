package remote

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestStdioServesScopedTools: the ssh forced-command transport serves the
// same scoped server. A read scope sees status and needs_you, land is
// refused, and both are audited under the ssh caller's name and address.
func TestStdioServesScopedTools(t *testing.T) {
	dir := t.TempDir()
	audit, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = audit.Close() }()
	var mu sync.Mutex
	var events []AuditEntry
	audit.Mirror = func(e AuditEntry) { mu.Lock(); events = append(events, e); mu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, ct := mcp.NewInMemoryTransports()
	c := StdioCaller([]Scope{ScopeRead}, "box", "203.0.113.9 50000 10.0.0.1 22")
	done := make(chan error, 1)
	go func() { done <- ServeStdio(ctx, fakeSource{}, audit, c, st) }()

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "ssh-test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("stdio read sees %v", names)
	}
	if res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "status"}); callErr(res, err) {
		t.Fatalf("status: %v %+v", err, res)
	}
	if res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "land"}); !callErr(res, err) {
		t.Fatal("land over stdio with a read scope succeeded")
	}
	_ = cs.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeStdio didn't return after the client closed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("audit = %+v", events)
	}
	for _, e := range events {
		if e.Token != "ssh:box" || e.Addr != "203.0.113.9" {
			t.Fatalf("stdio audit entry = %+v, want token ssh:box from 203.0.113.9", e)
		}
	}
}

func TestStdioCallerDefaults(t *testing.T) {
	c := StdioCaller([]Scope{ScopeRead}, "", "")
	if c.tok.Name != "ssh" || c.addr != "stdio" {
		t.Fatalf("caller = %+v", c)
	}
}

func TestStdioRefusesWhenOff(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := run(t, noOpen, "stdio", "--scope", "read")
	if err == nil || !strings.Contains(err.Error(), "remote control is off") {
		t.Fatalf("stdio with no config = %v, want it off", err)
	}
}

func TestStdioRefusesUnknownScope(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	writeFile(t, filepath.Join(cfg, "saddle", "config.toml"), "[remote]\nenabled = true\n")
	if _, err := run(t, noOpen, "stdio", "--scope", "root"); err == nil {
		t.Fatal("stdio accepted an unknown scope")
	}
}

func TestServeLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	release, err := AcquireServeLock(dir, "127.0.0.1:7431", "saddle up for /src/saddle")
	if err != nil {
		t.Fatal(err)
	}
	_, err = AcquireServeLock(dir, "127.0.0.1:7431", "saddle remote serve for /src/quark")
	if err == nil || !strings.Contains(err.Error(), "saddle up for /src/saddle") {
		t.Fatalf("second lock = %v, want it held by saddle up", err)
	}
	// Another address is another lock.
	other, err := AcquireServeLock(dir, "127.0.0.1:7432", "x")
	if err != nil {
		t.Fatal(err)
	}
	other()
	release()
	again, err := AcquireServeLock(dir, "127.0.0.1:7431", "y")
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}

type served struct {
	mu    sync.Mutex
	addrs []net.Addr
	logs  []string
}

func (s *served) onServe(a net.Addr) { s.mu.Lock(); s.addrs = append(s.addrs, a); s.mu.Unlock() }
func (s *served) logf(f string, args ...any) {
	s.mu.Lock()
	s.logs = append(s.logs, strings.TrimSpace(strings.ReplaceAll(f, "%", "")))
	s.mu.Unlock()
}

func (s *served) wait(t *testing.T, n int) net.Addr {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.addrs) >= n {
			a := s.addrs[n-1]
			s.mu.Unlock()
			return a
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server %d never started", n)
	return nil
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
}

func get(addr net.Addr) error {
	c := &http.Client{Timeout: time.Second}
	resp, err := c.Get("http://" + addr.String() + "/")
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

// TestServiceLifecycle: a managed server (the one saddle up and the engine
// run) waits while another process holds the lock, takes over when it
// stops, and on shutdown closes its listener and frees the lock.
func TestServiceLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Enabled: true, Listen: "127.0.0.1:0"}

	fg := &served{}
	fgCtx, stopFG := context.WithCancel(context.Background())
	fgDone := make(chan error, 1)
	go func() {
		fgDone <- (&Service{Cfg: cfg, Handler: okHandler(), Dir: dir, Holder: "saddle remote serve", OnServe: fg.onServe}).Run(fgCtx)
	}()
	fgAddr := fg.wait(t, 1)
	if err := get(fgAddr); err != nil {
		t.Fatalf("foreground server: %v", err)
	}

	// A second foreground serve fails at once, naming the holder.
	err := (&Service{Cfg: cfg, Handler: okHandler(), Dir: dir, Holder: "other"}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "saddle remote serve") {
		t.Fatalf("second serve = %v, want refused by the lock", err)
	}

	mg := &served{}
	mgCtx, stopMG := context.WithCancel(context.Background())
	mgDone := make(chan error, 1)
	go func() {
		mgDone <- (&Service{Cfg: cfg, Handler: okHandler(), Dir: dir, Holder: "saddle up", Wait: true, Retry: 10 * time.Millisecond,
			OnServe: mg.onServe, Logf: mg.logf}).Run(mgCtx)
	}()
	time.Sleep(50 * time.Millisecond)
	mg.mu.Lock()
	early, logged := len(mg.addrs), len(mg.logs)
	mg.mu.Unlock()
	if early != 0 {
		t.Fatal("managed server started while the lock was held")
	}
	if logged != 1 {
		t.Fatalf("managed server logged %d times while waiting, want once", logged)
	}

	stopFG()
	if err := <-fgDone; err != nil {
		t.Fatalf("foreground Run = %v", err)
	}
	if get(fgAddr) == nil {
		t.Fatal("foreground listener still open after shutdown")
	}
	mgAddr := mg.wait(t, 1)
	if err := get(mgAddr); err != nil {
		t.Fatalf("managed server after takeover: %v", err)
	}

	stopMG()
	select {
	case err := <-mgDone:
		if err != nil {
			t.Fatalf("managed Run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("managed server didn't stop")
	}
	if get(mgAddr) == nil {
		t.Fatal("managed listener still open after shutdown")
	}
	release, err := AcquireServeLock(dir, cfg.Listen, "after")
	if err != nil {
		t.Fatalf("lock not freed on shutdown: %v", err)
	}
	release()
}

// TestServiceRefusesBadConfig: the lifecycle checks the bind before it
// listens, so a public address without allow_public and TLS never opens.
func TestServiceRefusesBadConfig(t *testing.T) {
	err := (&Service{Cfg: Config{Enabled: true, Listen: "0.0.0.0:0"}, Handler: okHandler(), Dir: t.TempDir()}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "allow_public") {
		t.Fatalf("Run on 0.0.0.0 = %v", err)
	}
}

// TestRunManagedIsOffByDefault: saddle up and the engine call RunManaged
// on every start; with no [remote] config it returns at once and opens
// nothing (the app is never touched).
func TestRunManagedIsOffByDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	done := make(chan struct{})
	go func() {
		RunManaged(context.Background(), nil, func(string, ...any) { t.Error("RunManaged logged while off") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunManaged blocked with remote control off")
	}
}
