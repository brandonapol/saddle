package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// #764: while saddle's governor holds gh calls, the headline (saddle status,
// the TUI header) says "GitHub rate-limited until HH:MM", so the owner sees
// why nothing moves.
func TestHeadlineShowsGitHubRateLimit(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	now := time.Now()
	until := now.Add(30 * time.Minute)
	b, err := json.Marshal(forge.RateLimitHold{Until: until, Source: "gh pr view"})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(a.Root, ".saddle", forge.RateLimitFile), b, 0o644))
	h, err := a.Headline(now)
	must(t, err)
	want := "GitHub rate-limited until " + until.Local().Format("15:04")
	if h.Class != HeadlineDegraded || !strings.Contains(h.Text, want) {
		t.Fatalf("headline = %+v; want degraded with %q", h, want)
	}
	if h, _ := a.Headline(until.Add(time.Minute)); strings.Contains(h.Text, "rate-limited") {
		t.Fatalf("headline after the reset = %q", h.Text)
	}
}
