// Package remote lets Claude Code sessions other than the orchestrator's
// look at, and later drive, a running saddle (#251). It serves MCP over
// streamable HTTP on a loopback address, behind bearer tokens with scopes.
// It is off unless the user's own config turns it on. See
// docs/remote-control.md for the design and threat model.
package remote

import (
	"fmt"
	"slices"
	"strings"
)

// Scope is what a token may do. Scopes add up: a token holds a set of them.
type Scope string

const (
	// ScopeRead sees state: status, needs-you, briefs, peek.
	ScopeRead Scope = "read"
	// ScopeAct steers without landing: answer needs-you, message, hold and
	// release, queue order, automerge and concurrency toggles, unstack,
	// requeue, publish.
	ScopeAct Scope = "act"
	// ScopeLand moves the stack: land, prs, restack, sentinel_ack.
	ScopeLand Scope = "land"
	// ScopeAdmin is everything else that starts or stops work: spawn, kill,
	// autopilot. It implies every other scope.
	ScopeAdmin Scope = "admin"
	// ScopeLocal marks tools only saddle's own agents use (claim, done, …).
	// No token can hold it, so they are never reachable remotely.
	ScopeLocal Scope = "local"
)

// Grantable are the scopes a token can be given, least to most powerful.
var Grantable = []Scope{ScopeRead, ScopeAct, ScopeLand, ScopeAdmin}

// ToolScopes maps every orchestrator MCP tool, and every remote-only tool,
// to the scope that reaches it. It is an allowlist: a tool missing here is
// denied. TestEveryMCPToolHasAScope keeps it in step with mcpserver.
var ToolScopes = map[string]Scope{
	// read
	"status":    ScopeRead,
	"brief":     ScopeRead,
	"peek":      ScopeRead,
	"ticket":    ScopeRead,
	"needs_you": ScopeRead,
	// act
	"message":       ScopeAct,
	"send_keys":     ScopeAct,
	"queue_move":    ScopeAct,
	"queue_hold":    ScopeAct,
	"queue_release": ScopeAct,
	"automerge":     ScopeAct,
	"concurrency":   ScopeAct,
	"unstack":       ScopeAct,
	"requeue":       ScopeAct,
	"publish":       ScopeAct,
	// land
	"land":         ScopeLand,
	"prs":          ScopeLand,
	"restack":      ScopeLand,
	"sentinel_ack": ScopeLand,
	// admin
	"spawn":     ScopeAdmin,
	"kill":      ScopeAdmin,
	"autopilot": ScopeAdmin,
	// confirm runs an admin call held for confirmation (remote-only)
	ConfirmTool: ScopeAdmin,
	// local: a worker's own tools, meaningless from outside
	"claim":     ScopeLocal,
	"release":   ScopeLocal,
	"ask_owner": ScopeLocal,
	"done":      ScopeLocal,
}

// RemoteOnlyTools exist only on the remote server, not in mcpserver.
var RemoteOnlyTools = []string{"needs_you", ConfirmTool}

// Allows reports whether a token holding granted may call tool.
func Allows(granted []Scope, tool string) bool {
	need, ok := ToolScopes[tool]
	if !ok || need == ScopeLocal {
		return false
	}
	return slices.Contains(granted, need) || slices.Contains(granted, ScopeAdmin)
}

// ParseScopes reads a comma-separated scope list. Every token can read, so
// read is always added.
func ParseScopes(s string) ([]Scope, error) {
	out := []Scope{ScopeRead}
	for f := range strings.SplitSeq(s, ",") {
		sc := Scope(strings.TrimSpace(f))
		if sc == "" {
			continue
		}
		if !slices.Contains(Grantable, sc) {
			return nil, fmt.Errorf("scope %q: want one of read, act, land, admin", sc)
		}
		if !slices.Contains(out, sc) {
			out = append(out, sc)
		}
	}
	return out, nil
}
