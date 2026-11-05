package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/store"
)

// EventForgeRateLimited is logged once per GitHub rate-limit window, by the
// saddle process whose gh call found it (#764). Its data says until when
// gh calls are held and which call hit the limit.
const EventForgeRateLimited = "forge_rate_limited"

func init() { forge.OnRateLimited(logRateLimited) }

// logRateLimited records a new rate-limit window in root's event log. The
// forge layer runs in every saddle process (the engine, each agent's MCP
// server, hooks, the CLI), not all of which hold the store open, so it
// opens it for the one write; it never creates one.
func logRateLimited(root string, rl forge.RateLimitHold) {
	db := filepath.Join(root, ".saddle", "state.db")
	if _, err := os.Stat(db); errors.Is(err, os.ErrNotExist) {
		return
	}
	st, err := store.Open(db)
	if err != nil {
		return
	}
	defer st.Close()
	st.Event("", EventForgeRateLimited, fmt.Sprintf("GitHub rate-limited until %s (%s); gh calls wait for the reset",
		rl.Until.Local().Format("15:04"), rl.Source))
}
