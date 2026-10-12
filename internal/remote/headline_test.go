package remote

import (
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// The remote snapshot carries the same headline as status (#403), so the
// phone says what the TUI says.
func TestCompactCarriesHeadline(t *testing.T) {
	h := app.StateHeadline{Class: app.HeadlineBlocked, Color: app.ColorRed, Text: "Blocked: auto-merge stopped."}
	if s := Compact(mcpserver.StatusOut{Headline: h}, 0, "stopped"); s.Headline != h {
		t.Fatalf("snapshot headline = %+v, want %+v", s.Headline, h)
	}
}
