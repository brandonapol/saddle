package app

import (
	"os"
	"sync"
	"time"
)

// SymbolBudgetEnv, when set to a positive Go duration, replaces the time
// budget of every symbol rule (250ms for rules 1 and 2, 300ms for rule 3).
// Past its budget a rule keeps the path verdict, so on a loaded machine a
// share the rules would allow is denied. The e2e worlds set it, since the
// hook runs in saddle's own process where tests can't swap the budgets
// (#568).
const SymbolBudgetEnv = "SADDLE_SYMBOL_BUDGET"

// symbolBudgetFromEnv applies SymbolBudgetEnv once. It runs on first use,
// not in init, because the rules' own inits set the defaults after this
// file's would.
var symbolBudgetFromEnv = sync.OnceFunc(func() { applySymbolBudget(os.Getenv(SymbolBudgetEnv)) })

// applySymbolBudget sets every rule's budget to v, unless v is not a
// positive duration.
func applySymbolBudget(v string) {
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return
	}
	shareBudget.Store(int64(d))
	staleBudget.Store(int64(d))
	assumeBudget.Store(int64(d))
}
