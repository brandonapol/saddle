package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath is the only path the server answers.
const MCPPath = "/mcp"

const instructions = "Remote view of a running saddle. status is a compact snapshot (agents, train, stack, CI holds); " +
	"needs_you lists what waits on the owner: agents at a prompt with the prompt's text, and the orchestrator's action notices. " +
	"This server is read-only for now: act on what you see from the host's orchestrator session."

// implemented are the tools this server serves today. Everything else in
// ToolScopes is denied even to a token scoped for it, until it has
// remote-safe semantics (#251 follow-ups).
var implemented = map[string]bool{"status": true, "needs_you": true}

// Limits are the server's rate limits.
type Limits struct {
	PerMinute      int // calls per token
	FailsPerMinute int // failed logins per client address
}

type ctxKey struct{}

// caller is who a request authenticated as.
type caller struct {
	tok  Token
	addr string
}

// Handler serves MCP at MCPPath for bearer-token callers. Requests from
// browsers (an Origin header) or for a non-loopback Host (DNS rebinding)
// are refused before auth; every failed login and every tool call is
// audited.
func Handler(src Source, tokens *Tokens, audit *Audit, lim Limits) http.Handler {
	perTok, fails := newWindow(lim.PerMinute), newWindow(lim.FailsPerMinute)
	mcpH := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		c, _ := r.Context().Value(ctxKey{}).(caller)
		return newServer(src, c, audit)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser requests are not accepted", http.StatusForbidden)
			return
		}
		if !loopbackHost(hostOnly(r.Host)) {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		if r.URL.Path != MCPPath {
			http.NotFound(w, r)
			return
		}
		addr := hostOnly(r.RemoteAddr)
		if fails.full(addr) {
			http.Error(w, "too many failed logins; wait a minute", http.StatusTooManyRequests)
			return
		}
		tok, err := tokens.Verify(bearerToken(r))
		if err != nil {
			fails.hit(addr)
			reason := "bad token"
			if !errors.Is(err, ErrBadToken) {
				reason = err.Error()
			}
			_ = audit.Log(AuditEntry{Addr: addr, Decision: DecisionDenied, Reason: reason})
			w.Header().Set("WWW-Authenticate", `Bearer realm="saddle"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !perTok.hit(tok.Name) {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		mcpH.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, caller{tok: tok, addr: addr})))
	})
}

func bearerToken(r *http.Request) string {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// NeedsYouOut is the needs_you tool's result.
type NeedsYouOut struct {
	Items []NeedsYouItem `json:"items"`
}

// newServer builds the MCP server one caller sees: only the tools its
// scopes reach, behind a middleware that checks and audits every call.
func newServer(src Source, c caller, audit *Audit) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "saddle-remote", Version: "0.1.0"}, &mcp.ServerOptions{Instructions: instructions})
	if Allows(c.tok.Scopes, "status") {
		mcp.AddTool(s, &mcp.Tool{Name: "status", Description: "Compact snapshot of the running saddle: counts, live tasks (needs-you first), merge train, stack at risk, CI-red holds, automerge."},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, Snapshot, error) {
				out, err := src.Snapshot()
				return nil, out, err
			})
	}
	if Allows(c.tok.Scopes, "needs_you") {
		mcp.AddTool(s, &mcp.Tool{Name: "needs_you", Description: "What waits on the owner: agents at a permission prompt or question (with its text), and the orchestrator's undelivered action notices. Reading does not consume them."},
			func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, NeedsYouOut, error) {
				items, err := src.NeedsYou()
				if items == nil {
					items = []NeedsYouItem{}
				}
				return nil, NeedsYouOut{Items: items}, err
			})
	}
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			name := ""
			if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
				name = p.Name
			}
			e := AuditEntry{Token: c.tok.Name, Addr: c.addr, Tool: name, Decision: DecisionDenied}
			switch {
			case !Allows(c.tok.Scopes, name):
				e.Reason = fmt.Sprintf("token %q lacks the scope for %q", c.tok.Name, name)
			case !implemented[name]:
				e.Reason = fmt.Sprintf("%q is not served remotely yet", name)
			default:
				e.Decision = DecisionAllowed
			}
			if err := audit.Log(e); err != nil {
				return nil, fmt.Errorf("audit log unavailable, refusing %s: %w", name, err)
			}
			if e.Decision == DecisionDenied {
				return nil, errors.New(e.Reason)
			}
			return next(ctx, method, req)
		}
	})
	return s
}

// window is a fixed one-minute rate limit per key.
type window struct {
	mu    sync.Mutex
	limit int
	now   func() time.Time
	seen  map[string]*slot
}

type slot struct {
	start time.Time
	n     int
}

func newWindow(limit int) *window {
	return &window{limit: limit, now: time.Now, seen: map[string]*slot{}}
}

func (w *window) get(key string) *slot {
	now := w.now()
	s := w.seen[key]
	if s == nil || now.Sub(s.start) >= time.Minute {
		s = &slot{start: now}
		w.seen[key] = s
	}
	return s
}

// hit counts one use of key and reports whether it was within the limit.
func (w *window) hit(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.get(key)
	s.n++
	return s.n <= w.limit
}

// full reports whether key has used up this minute.
func (w *window) full(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.get(key).n >= w.limit
}
