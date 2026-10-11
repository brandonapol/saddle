package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPPath is the only path the server answers.
const MCPPath = "/mcp"

const instructions = "Remote view of a running saddle. status is a compact snapshot (agents, train, stack, CI holds); " +
	"needs_you lists what waits on the owner: agents at a prompt with the prompt's text, and the orchestrator's action notices. " +
	"This server is read-only for now: act on what you see from the host's orchestrator session. " +
	"Admin tools, when served, run only after a confirm call with the code the first call returns: show the owner what will run before confirming."

// Limits are the server's rate limits.
type Limits struct {
	PerMinute      int // calls per token
	FailsPerMinute int // failed logins per client address
}

// Options configure Handler.
type Options struct {
	Limits Limits
	// Repo is the root of the repo served. A token with a repo allowlist
	// that doesn't name it is refused.
	Repo string
	// Hosts are Host header values accepted besides loopback ones (see
	// Config.AllowedHosts).
	Hosts []string

	extra []toolDef        // tests serve extra tools (a fake admin tool)
	now   func() time.Time // tests move the confirmation clock
}

type ctxKey struct{}

// caller is who a request authenticated as.
type caller struct {
	tok  Token
	addr string
}

// Handler serves MCP at MCPPath for bearer-token callers. Requests from
// browsers (an Origin header) or for a Host that isn't loopback or listed
// (DNS rebinding) are refused before auth; every failed login and every
// tool call is audited.
func Handler(src Source, tokens *Tokens, audit *Audit, opts Options) http.Handler {
	perTok, fails := newWindow(opts.Limits.PerMinute), newWindow(opts.Limits.FailsPerMinute)
	st := newServerState(opts)
	mcpH := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		c, _ := r.Context().Value(ctxKey{}).(caller)
		return newServer(src, c, audit, st)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true,
		// The SDK refuses any Host but loopback on a loopback socket. The
		// check below does the same unless hosts were listed (a tailnet
		// proxy in front sends its own name), so it alone decides then.
		DisableLocalhostProtection: len(opts.Hosts) > 0})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "browser requests are not accepted", http.StatusForbidden)
			return
		}
		if h := hostOnly(r.Host); !loopbackHost(h) && !slices.ContainsFunc(opts.Hosts, func(x string) bool { return strings.EqualFold(x, h) }) {
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
		if !tok.AllowsRepo(opts.Repo) {
			_ = audit.Log(AuditEntry{Token: tok.Name, Addr: addr, Decision: DecisionDenied,
				Reason: fmt.Sprintf("token %q isn't allowed for repo %s", tok.Name, filepath.Base(opts.Repo))})
			http.Error(w, "this token isn't allowed for this repo", http.StatusForbidden)
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

// toolDef is a tool the remote server serves. add registers it on a
// server; newServer only calls add for a caller whose scopes reach it.
type toolDef struct {
	name string
	add  func(s *mcp.Server)
}

func builtinTools(src Source) []toolDef {
	return []toolDef{
		{"status", func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{Name: "status", Description: "Compact snapshot of the running saddle: counts, live tasks (needs-you first), merge train, stack at risk, CI-red holds, automerge."},
				func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, Snapshot, error) {
					out, err := src.Snapshot()
					return nil, out, err
				})
		}},
		{"needs_you", func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{Name: "needs_you", Description: "What waits on the owner: agents at a permission prompt or question (with its text), and the orchestrator's undelivered action notices. Reading does not consume them."},
				func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, NeedsYouOut, error) {
					items, err := src.NeedsYou()
					if items == nil {
						items = []NeedsYouItem{}
					}
					return nil, NeedsYouOut{Items: items}, err
				})
		}},
	}
}

// serverState outlives one MCP session: the confirmations an admin caller
// has pending, since the HTTP transport is stateless.
type serverState struct {
	extra    []toolDef
	confirms *confirmations
}

func newServerState(opts Options) *serverState {
	now := opts.now
	if now == nil {
		now = time.Now
	}
	return &serverState{extra: opts.extra, confirms: newConfirmations(now)}
}

// newServer builds the MCP server one caller sees: only the tools its
// scopes reach, behind a middleware that checks and audits every call and
// holds admin calls until they are confirmed.
func newServer(src Source, c caller, audit *Audit, st *serverState) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "saddle-remote", Version: "0.2.0"}, &mcp.ServerOptions{Instructions: instructions})
	served := map[string]bool{}
	for _, t := range append(builtinTools(src), st.extra...) {
		served[t.name] = true
		if Allows(c.tok.Scopes, t.name) {
			t.add(s)
		}
	}
	if Allows(c.tok.Scopes, ConfirmTool) {
		served[ConfirmTool] = true
		addConfirmTool(s)
	}
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			p, _ := req.GetParams().(*mcp.CallToolParamsRaw)
			name := ""
			if p != nil {
				name = p.Name
			}
			e := AuditEntry{Token: c.tok.Name, Addr: c.addr, Tool: name, Decision: DecisionDenied}
			switch {
			case !Allows(c.tok.Scopes, name):
				e.Reason = fmt.Sprintf("token %q lacks the scope for %q", c.tok.Name, name)
			case !served[name]:
				e.Reason = fmt.Sprintf("%q is not served remotely yet", name)
			case name == ConfirmTool:
				return confirmCall(ctx, c, audit, st.confirms, req, p, next)
			case NeedsConfirm(name):
				return holdForConfirm(c, audit, st.confirms, p)
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
