package cli

import (
	"strings"
	"testing"
)

// #176: saddle concurrency shows the cap, N overrides it at runtime, reset
// drops the override, and out-of-range values are refused.
func TestConcurrencyCommand(t *testing.T) {
	automergeRepo(t) // t1 is running
	must := func(args ...string) string {
		t.Helper()
		out, err := runCmd(t, concurrencyCmd(), args...)
		if err != nil {
			t.Fatalf("saddle concurrency %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	if out := must(); !strings.Contains(out, "bots: 1 running, limit 5 (config)") {
		t.Fatalf("default:\n%s", out)
	}
	if out := must("3"); !strings.Contains(out, "limit 3 (runtime)") {
		t.Fatalf("set 3:\n%s", out)
	}
	if out := must("--json"); !strings.Contains(out, `"limit": 3`) || !strings.Contains(out, `"running": 1`) {
		t.Fatalf("json:\n%s", out)
	}
	for _, bad := range []string{"0", "17", "lots"} {
		if out, err := runCmd(t, concurrencyCmd(), bad); err == nil {
			t.Errorf("saddle concurrency %s succeeded:\n%s", bad, out)
		}
	}
	if out := must("reset"); !strings.Contains(out, "limit 5 (config)") {
		t.Fatalf("reset:\n%s", out)
	}
}

func TestRootRegistersConcurrency(t *testing.T) {
	if c, _, err := Root().Find([]string{"concurrency"}); err != nil || c.Name() != "concurrency" {
		t.Fatalf("saddle concurrency not registered: %v", err)
	}
}
