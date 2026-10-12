package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/doctor"
)

// decisionPredictedRisks is the kind of the card saddle up opens for the
// risks doctor --predict found (#408).
const decisionPredictedRisks = "predicted-risks"

// upPredictTimeout bounds the prediction at saddle up; it runs in the
// background, so a slow probe only delays the card.
const upPredictTimeout = 30 * time.Second

// runPredict is saddle doctor --predict. It opens the repo's state only when
// saddle init has run, for the escalation history.
func runPredict(out io.Writer, root string, asJSON bool) error {
	var a *app.App
	if _, err := os.Stat(filepath.Join(root, ".saddle", "state.db")); err == nil {
		if a, err = app.Open(root); err == nil {
			defer a.Close()
		}
	}
	rs := doctor.Predict(doctor.PredictSystem(root, a), time.Now())
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if rs == nil {
			rs = []doctor.Risk{}
		}
		return enc.Encode(rs)
	}
	doctor.WriteRisks(out, rs)
	return nil
}

// upPredict runs predict in the background for saddle up and opens one
// decision card for what it finds at Medium or above. It never blocks up:
// it returns at once, and the returned wait (for tests) blocks until the
// card is opened or the prediction gave up after timeout.
func upPredict(a *app.App, timeout time.Duration, predict func() []doctor.Risk) (wait func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		got := make(chan []doctor.Risk, 1)
		go func() { got <- predict() }()
		select {
		case rs := <-got:
			if c, ok := predictedRisksCard(rs); ok {
				if err := a.OpenDecision(c); err != nil {
					a.Store.Event(app.OrchestratorID, "predict_error", err.Error())
				}
			}
		case <-time.After(timeout):
			a.Store.Event(app.OrchestratorID, "predict_error", "doctor --predict timed out after "+timeout.String())
		}
	}()
	return func() { <-done }
}

// upPredictSystem is the prediction saddle up runs: the real probes, then
// it records the Claude Code version so the next run can spot drift.
func upPredictSystem(a *app.App) func() []doctor.Risk {
	return func() []doctor.Risk {
		env := doctor.PredictSystem(a.Root, a)
		rs := doctor.Predict(env, time.Now())
		if v, _ := env.ClaudeVersions(); v != "" {
			_ = doctor.RecordClaudeVersion(a.Root, v)
		}
		return rs
	}
}

// predictedRisksCard is the one card for the risks worth the owner's
// attention before walking away, or false when none is Medium or above.
// Its ID hashes the risks, so a second saddle up with the same risks while
// the card is open doesn't send it again.
func predictedRisksCard(rs []doctor.Risk) (app.DecisionCard, bool) {
	var top []doctor.Risk
	high := 0
	for _, r := range rs {
		if r.Severity >= doctor.Medium {
			top = append(top, r)
		}
		if r.Severity == doctor.High {
			high++
		}
	}
	if len(top) == 0 {
		return app.DecisionCard{}, false
	}
	h := sha256.New()
	var what []string
	for i, r := range top {
		fmt.Fprintln(h, r.Line())
		if i < 3 {
			what = append(what, fmt.Sprintf("%s: %s", r.Severity, r.What))
		}
	}
	if len(top) > 3 {
		what = append(what, fmt.Sprintf("and %d more", len(top)-3))
	}
	headline := fmt.Sprintf("Before you walk away: %d risk(s) could stop saddle", len(top))
	if high > 0 {
		headline += fmt.Sprintf(", %d of them High", high)
	}
	fix := oneLineFix(top[0].Fix)
	return app.DecisionCard{
		ID:       decisionPredictedRisks + ":" + hex.EncodeToString(h.Sum(nil))[:12],
		Kind:     decisionPredictedRisks,
		To:       app.OrchestratorID,
		Headline: headline + ".",
		What:     strings.Join(what, "; ") + ".",
		Blocking: "Nothing yet: agents run as normal. `saddle doctor --predict` lists every risk with its fix.",
		Options: []app.DecisionOption{
			{Label: "Fix the top risk now", Cost: "you do it, a few minutes", Action: fix, Recommended: high > 0},
			{Label: "Carry on and keep an eye on it", Cost: "saddle keeps running; the risk stays", Action: "saddle doctor --predict", Recommended: high == 0},
		},
		Default: 2,
		Timeout: app.DefaultDecisionTimeout,
	}, true
}

// oneLineFix squeezes a fix onto one line for a card's action.
func oneLineFix(fix string) string { return strings.Join(strings.Fields(fix), " ") }
