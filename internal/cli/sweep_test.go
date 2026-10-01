package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
)

// recordGH records gh calls and lists one ready PR.
func recordGH(calls *[]string) func(context.Context, ...string) (string, error) {
	return func(_ context.Context, args ...string) (string, error) {
		k := strings.Join(args, " ")
		*calls = append(*calls, k)
		if strings.HasPrefix(k, "pr list --state open") {
			return `[{"number":7,"headRefName":"saddle/t1-a","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE",
"files":[{"path":"a.go"},{"path":"a_test.go"}],"statusCheckRollup":[{"name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}]`, nil
		}
		return "", nil
	}
}

func TestSweepDisabledByDefault(t *testing.T) {
	var calls []string
	var out bytes.Buffer
	if err := runSweep(context.Background(), &out, config.Default(), false, recordGH(&calls), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sweeper is disabled") || len(calls) != 0 {
		t.Fatalf("out %q, calls %q", out.String(), calls)
	}
}

func TestSweepDryRunWorksWhenDisabled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		flag   bool
		config bool
	}{{"flag", true, false}, {"config", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Sweeper.DryRun = tc.config
			var calls []string
			var out bytes.Buffer
			logged := 0
			err := runSweep(context.Background(), &out, cfg, tc.flag, recordGH(&calls), func(string, string, string) { logged++ })
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || logged != 0 {
				t.Fatalf("dry run made calls %q, logged %d", calls, logged)
			}
			if !strings.Contains(out.String(), "would merge (1)") {
				t.Fatalf("report: %q", out.String())
			}
		})
	}
}

func TestSweepEnabledMerges(t *testing.T) {
	cfg := config.Default()
	cfg.Sweeper.Enabled = true
	var calls []string
	var out bytes.Buffer
	var kinds []string
	err := runSweep(context.Background(), &out, cfg, false, recordGH(&calls), func(_, kind, _ string) { kinds = append(kinds, kind) })
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1] != "pr merge 7 --squash --match-head-commit abc" {
		t.Fatalf("calls %q", calls)
	}
	if len(kinds) != 1 || kinds[0] != "sweep.merged" || !strings.Contains(out.String(), "merged (1)") {
		t.Fatalf("kinds %q, out %q", kinds, out.String())
	}
}

func TestSweepRegistered(t *testing.T) {
	c, _, err := Root().Find([]string{"sweep"})
	if err != nil || c.Name() != "sweep" || c.Flags().Lookup("dry-run") == nil {
		t.Fatalf("sweep command not registered: %v", err)
	}
}
