package app

import (
	"testing"
	"time"
)

// TestSymbolBudgetEnvWidensEveryRule (#568): the hook runs in saddle's own
// process, so an e2e journey can't call SetShareBudget. SymbolBudgetEnv
// widens all three rule budgets; a bad or empty value keeps the defaults.
func TestSymbolBudgetEnvWidensEveryRule(t *testing.T) {
	t.Cleanup(SetShareBudget(250 * time.Millisecond))
	t.Cleanup(SetStaleBudget(250 * time.Millisecond))
	t.Cleanup(SetAssumeBudget(300 * time.Millisecond))
	budgets := func() []time.Duration {
		return []time.Duration{time.Duration(shareBudget.Load()), time.Duration(staleBudget.Load()), time.Duration(assumeBudget.Load())}
	}
	for _, v := range []string{"", "soon", "0s", "-1s"} {
		applySymbolBudget(v)
		if got := budgets(); got[0] != 250*time.Millisecond || got[1] != 250*time.Millisecond || got[2] != 300*time.Millisecond {
			t.Fatalf("%s=%q changed the budgets to %v", SymbolBudgetEnv, v, got)
		}
	}
	applySymbolBudget("30s")
	for i, got := range budgets() {
		if got != 30*time.Second {
			t.Fatalf("budget %d = %v after %s=30s, want 30s", i, got, SymbolBudgetEnv)
		}
	}
}
