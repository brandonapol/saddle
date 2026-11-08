package app

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/forge"
)

// IssueClaims are the claims a spawn for issue n starts with when the
// orchestrator gives none (#908): the globs the ticket's Claims: line (or
// Claims section) declares, else the ones autopilot infers from the paths
// it names. Inferred globs another live task holds are left out, since the
// guess must not block the spawn; declared ones stay, so an overlap is
// refused naming its owner. note says where the claims came from, for the
// orchestrator; nil claims with a note means the worker claims as it goes.
func (a *App) IssueClaims(n int) (globs []string, note string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	is, err := a.Forge().Issue(ctx, "", n, forge.IssueBody)
	if err != nil {
		return nil, fmt.Sprintf("couldn't read #%d for its claims (%v); the worker claims files as it goes", n, err)
	}
	globs, inferred := autopilot.TicketClaims(is.Body, os.DirFS(a.Root))
	if len(globs) == 0 {
		return nil, fmt.Sprintf("#%d declares no Claims: line and names no repo paths; the worker claims files as it goes", n)
	}
	if !inferred {
		return globs, fmt.Sprintf("claims from #%d's Claims: line: %s", n, strings.Join(globs, ", "))
	}
	held := map[string]string{}
	if all, err := a.Store.Claims(); err == nil {
		held = claims.Conflicts(all, "", globs)
	}
	var kept, dropped []string
	for _, g := range globs {
		if owner, ok := held[g]; ok {
			dropped = append(dropped, g+" (held by "+owner+")")
			continue
		}
		kept = append(kept, g)
	}
	note = fmt.Sprintf("#%d has no Claims: line; claims inferred from the paths it names: %s", n, strings.Join(kept, ", "))
	if len(kept) == 0 {
		note = fmt.Sprintf("#%d has no Claims: line, and every path it names is claimed already", n)
	}
	if len(dropped) > 0 {
		slices.Sort(dropped)
		note += "; left out, already claimed: " + strings.Join(dropped, ", ")
	}
	return kept, note
}
