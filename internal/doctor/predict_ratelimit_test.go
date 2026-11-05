package doctor

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// heldGH is the quiet machine with saddle's rate-limit governor holding gh
// calls (#764).
type heldGH struct {
	*fakePredict
	until time.Time
}

func (h heldGH) GH(args ...string) (string, error) {
	return "", fmt.Errorf("gh %s: %w", strings.Join(args, " "), &forge.RateLimitError{Until: h.until})
}

// #764: while GitHub is rate-limited, doctor --predict says so up front:
// "GitHub rate-limited until HH:MM".
func TestPredictShowsRateLimitHold(t *testing.T) {
	until := time.Date(2026, 11, 5, 14, 37, 0, 0, time.Local)
	rs := Predict(heldGH{quiet(), until}, time.Now())
	if len(rs) == 0 || rs[0].Class != ClassRateLimit || rs[0].Severity != High ||
		!strings.Contains(rs[0].What, "GitHub rate-limited until 14:37") {
		t.Fatalf("risks = %+v; want a High rate-limit hold first", rs)
	}
}
