//go:build e2e

// Package e2e holds remote-control journeys (#317): the real saddle binary
// serving `saddle remote serve` on a real loopback listener, driven by MCP
// clients over streamable HTTP. It reuses internal/e2e's World; run it with
// `make test/e2e`.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	harness "github.com/brandonapol/saddle/internal/e2e"
	"github.com/brandonapol/saddle/internal/remote"
)

var bins harness.Bins

// TestMain builds the binaries once and runs each journey in its own
// process, in parallel (see harness.Main and harness.Fork).
func TestMain(m *testing.M) { harness.Main(m, &bins) }

// host is a World with remote control on and `saddle remote serve`
// listening on a kernel-chosen loopback port.
type host struct {
	*harness.World
	URL string // the repo's /mcp/<name> endpoint
}

var servingRe = regexp.MustCompile(`saddle remote: serving \S+ at (http://\S+)`)

// newHost builds a World, turns remote control on in its user config and
// starts the listener, stopping it when t ends.
func newHost(t *testing.T) *host {
	t.Helper()
	w := harness.New(t, bins, harness.Options{})
	t.Cleanup(w.Diagnose)
	cfgDir := filepath.Join(w.Home, ".config", "saddle")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"),
		[]byte("[remote]\nenabled = true\nlisten = \"127.0.0.1:0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(w.Bins.Saddle, "remote", "serve")
	cmd.Dir, cmd.Env = w.Repo, w.Env()
	out := &syncBuf{}
	cmd.Stderr = out
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(out, bufio.NewReader(stdout))
	}()
	exited := make(chan error, 1)
	go func() {
		<-copied
		exited <- cmd.Wait()
	}()
	// Runs before the World's own cleanups: the server goes before its
	// HOME and tmux do.
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		if err := harness.Poll(harness.Timeout(), func() error {
			select {
			case <-exited:
				return nil
			default:
				return errors.New("saddle remote serve still running")
			}
		}); err != nil {
			_ = cmd.Process.Kill()
			<-exited
		}
		if t.Failed() {
			t.Logf("saddle remote serve output:\n%s", out.String())
		}
	})

	h := &host{World: w}
	harness.Eventually(t, "saddle remote serve to listen", func() error {
		select {
		case err := <-exited:
			t.Fatalf("saddle remote serve exited: %v\n%s", err, out.String())
		default:
		}
		m := servingRe.FindStringSubmatch(out.String())
		if m == nil {
			return fmt.Errorf("no serving line yet:\n%s", out.String())
		}
		h.URL = m[1]
		return nil
	})
	return h
}

// token mints a token with the host's CLI and returns its secret, which
// exists only at runtime.
func (h *host) token(name, scopes string) string {
	h.T.Helper()
	r := h.MustSaddle("remote", "token", "create", name, "--scope", scopes, "--ttl", "1h")
	secret := strings.TrimSpace(r.Stdout)
	if secret == "" {
		h.T.Fatalf("token create %s printed no secret: %s", name, r)
	}
	return secret
}

// connect opens an MCP session to the listener with secret as the bearer.
func (h *host) connect(secret string) (*mcp.ClientSession, error) {
	tr := &mcp.StreamableClientTransport{Endpoint: h.URL, HTTPClient: &http.Client{Transport: bearer(secret)},
		MaxRetries: -1, DisableStandaloneSSE: true}
	ctx, cancel := context.WithTimeout(context.Background(), harness.Timeout())
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e-remote"}, nil).Connect(ctx, tr, nil)
	if err == nil {
		h.T.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func (h *host) mustConnect(secret string) *mcp.ClientSession {
	h.T.Helper()
	cs, err := h.connect(secret)
	if err != nil {
		h.T.Fatalf("connect: %v", err)
	}
	return cs
}

// audit is the host's audit log, read with `saddle remote audit`.
func (h *host) audit() []remote.AuditEntry {
	h.T.Helper()
	r := h.MustSaddle("remote", "audit", "--json", "--limit", "0")
	var es []remote.AuditEntry
	dec := json.NewDecoder(strings.NewReader(r.Stdout))
	for dec.More() {
		var e remote.AuditEntry
		if err := dec.Decode(&e); err != nil {
			h.T.Fatalf("audit line: %v\n%s", err, r.Stdout)
		}
		es = append(es, e)
	}
	return es
}

// waitAudit waits for an audit entry matching ok.
func (h *host) waitAudit(what string, ok func(remote.AuditEntry) bool) remote.AuditEntry {
	h.T.Helper()
	var got remote.AuditEntry
	harness.Eventually(h.T, "audit entry: "+what, func() error {
		es := h.audit()
		for _, e := range es {
			if ok(e) {
				got = e
				return nil
			}
		}
		return fmt.Errorf("none in %d entries: %+v", len(es), es)
	})
	return got
}

type bearer string

func (b bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(req)
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// call runs a tool; err covers both a transport error and an error result.
func call(cs *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), harness.Timeout())
	defer cancel()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return res, err
	}
	if res.IsError {
		return res, errors.New(text(res))
	}
	return res, nil
}

func text(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var s []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			s = append(s, t.Text)
		}
	}
	return strings.Join(s, "\n")
}

// decode reads a result's structured content into v.
func decode(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		t.Fatalf("decode %s: %v", text(res), err)
	}
}

// status is the remote status snapshot.
func status(t *testing.T, cs *mcp.ClientSession) remote.Snapshot {
	t.Helper()
	res, err := call(cs, "status", nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var s remote.Snapshot
	decode(t, res, &s)
	return s
}

// hold makes the first half of the confirmation round trip and returns
// the code.
func hold(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := call(cs, tool, args)
	if err != nil {
		t.Fatalf("%s %v: %v", tool, args, err)
	}
	var c remote.ConfirmRequired
	decode(t, res, &c)
	if !c.Confirm || c.Code == "" {
		t.Fatalf("%s %v wasn't held for confirmation: %s", tool, args, text(res))
	}
	return c.Code
}

func confirm(cs *mcp.ClientSession, code string) (*mcp.CallToolResult, error) {
	return call(cs, remote.ConfirmTool, map[string]any{"code": code})
}
