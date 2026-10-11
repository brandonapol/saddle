package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ConfirmTool is the second half of the confirmation round trip for admin
// tools: the first call runs nothing and returns a code, and confirm with
// that code runs the call as it was first sent.
const ConfirmTool = "confirm"

// ConfirmTTL is how long a held admin call waits for its confirm.
const ConfirmTTL = 2 * time.Minute

// NeedsConfirm reports whether tool is held for confirmation: every
// mutating tool except confirm itself and the panic pause.
func NeedsConfirm(tool string) bool {
	return tool != ConfirmTool && tool != PauseTool && Mutating(tool)
}

// ConfirmRequired is what a held admin call returns instead of running.
type ConfirmRequired struct {
	Confirm   bool            `json:"confirm_required"`
	Code      string          `json:"code" jsonschema:"pass this to the confirm tool to run the call"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Expires   time.Time       `json:"expires"`
}

type pendingCall struct {
	tokHash string // bound to the token, not just its name: a re-issued token can't confirm
	repo    string // and to the repo it was held on
	tool    string
	args    json.RawMessage
	expires time.Time
}

// confirmations are the admin calls held for confirmation. Codes are one
// time and expire after ConfirmTTL.
type confirmations struct {
	mu      sync.Mutex
	now     func() time.Time
	pending map[string]pendingCall
}

func newConfirmations(now func() time.Time) *confirmations {
	return &confirmations{now: now, pending: map[string]pendingCall{}}
}

func (cs *confirmations) hold(tokHash, repo, tool string, args json.RawMessage) (string, time.Time, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	code := hex.EncodeToString(raw)
	cs.mu.Lock()
	defer cs.mu.Unlock()
	now := cs.now()
	for k, p := range cs.pending {
		if !now.Before(p.expires) {
			delete(cs.pending, k)
		}
	}
	exp := now.Add(ConfirmTTL)
	cs.pending[code] = pendingCall{tokHash: tokHash, repo: repo, tool: tool, args: args, expires: exp}
	return code, exp, nil
}

// take removes and returns the call held under code for this token and
// repo. A code held for another token or repo is left alone for its owner.
func (cs *confirmations) take(tokHash, repo, code string) (pendingCall, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	p, ok := cs.pending[code]
	if !ok || p.tokHash != tokHash || p.repo != repo {
		return pendingCall{}, fmt.Errorf("no pending call with code %q for this token", code)
	}
	delete(cs.pending, code)
	if !cs.now().Before(p.expires) {
		return pendingCall{}, fmt.Errorf("code %q expired; call the tool again for a new one", code)
	}
	return p, nil
}

type confirmIn struct {
	Code string `json:"code" jsonschema:"the code an admin tool returned"`
}

// maxAuditArgs caps the arguments an audit entry records.
const maxAuditArgs = 300

// auditArgs is a call's arguments for the audit log: scrubbed and clipped.
func auditArgs(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return scrubText(string(raw), maxAuditArgs)
}

func addConfirmTool(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: ConfirmTool, Description: "Run a call held for confirmation. Every tool that changes state (answer, queue_hold, land, kill, …) runs nothing on the first call; " +
		"they return a code. Show the owner what will run, then call confirm with the code within " + ConfirmTTL.String() + ". A code works once."},
		func(context.Context, *mcp.CallToolRequest, confirmIn) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, errors.New("confirm is handled by the server's middleware") // never reached
		})
}

// holdForConfirm audits a mutating call as pending and returns its code.
func holdForConfirm(c caller, audit *Audit, cs *confirmations, p *mcp.CallToolParamsRaw) (mcp.Result, error) {
	code, exp, err := cs.hold(c.tok.Hash, c.repo, p.Name, p.Arguments)
	if err != nil {
		return nil, err
	}
	if err := audit.Log(AuditEntry{Token: c.tok.Name, Addr: c.addr, Repo: c.repo, Tool: p.Name, Decision: DecisionPending,
		Args: auditArgs(p.Arguments), Reason: "held for confirmation, code " + code}); err != nil {
		return nil, fmt.Errorf("audit log unavailable, refusing %s: %w", p.Name, err)
	}
	args := string(p.Arguments)
	if args == "" {
		args = "{}"
	}
	msg := fmt.Sprintf("Not run yet: %s %s needs confirmation. Show the owner exactly what will run; "+
		"to go ahead, call confirm with code %s before %s. The code works once.", p.Name, args, code, exp.UTC().Format(time.TimeOnly+" UTC"))
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: msg}},
		StructuredContent: ConfirmRequired{Confirm: true, Code: code, Tool: p.Name, Arguments: p.Arguments, Expires: exp},
	}, nil
}

// confirmCall runs the call held under the given code, as first sent, and
// audits it as allowed. A bad, expired, reused or foreign code is denied.
func confirmCall(ctx context.Context, c caller, audit *Audit, cs *confirmations, req mcp.Request, p *mcp.CallToolParamsRaw, next mcp.MethodHandler) (mcp.Result, error) {
	var in confirmIn
	if p != nil && len(p.Arguments) > 0 {
		_ = json.Unmarshal(p.Arguments, &in)
	}
	pc, err := cs.take(c.tok.Hash, c.repo, in.Code)
	e := AuditEntry{Token: c.tok.Name, Addr: c.addr, Repo: c.repo, Tool: ConfirmTool, Decision: DecisionDenied}
	switch {
	case err != nil:
		e.Reason = err.Error()
	case !RemoteAllows(c.tok.Scopes, pc.tool):
		e.Reason = fmt.Sprintf("token %q lacks the scope for %q", c.tok.Name, pc.tool)
	default:
		e = AuditEntry{Token: c.tok.Name, Addr: c.addr, Repo: c.repo, Tool: pc.tool, Decision: DecisionAllowed,
			Args: auditArgs(pc.args), Reason: "confirmed, code " + in.Code}
	}
	if err := audit.Log(e); err != nil {
		return nil, fmt.Errorf("audit log unavailable, refusing %s: %w", e.Tool, err)
	}
	if e.Decision == DecisionDenied {
		return nil, errors.New(e.Reason)
	}
	orig, _ := req.(*mcp.CallToolRequest)
	call := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: pc.tool, Arguments: pc.args}}
	if orig != nil {
		call.Session, call.Extra = orig.Session, orig.Extra
	}
	return next(ctx, "tools/call", call)
}
