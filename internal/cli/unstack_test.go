package cli

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

func run(t *testing.T, args ...string) string {
	t.Helper()
	var out strings.Builder
	cmd := Root()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("saddle %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// #119.5: the escape hatches are CLI commands, so nobody edits state.db:
// sentinel check, sentinel ack and unstack by PR number.
func TestSentinelAckAndUnstackCommands(t *testing.T) {
	a, _ := landedPR(t) // gh reports t1's PR as conflicting
	t.Chdir(a.Root)

	if out := run(t, "sentinel", "check"); !strings.Contains(out, "stack at risk from t1 up") {
		t.Fatalf("sentinel check:\n%s", out)
	}
	if out := run(t, "sentinel", "ack"); !strings.Contains(out, "acknowledged the flag from t1 up") {
		t.Fatalf("sentinel ack:\n%s", out)
	}
	if f, _, _ := a.Flag(); !f.Acked || len(f.PRs) != 0 {
		t.Fatalf("flag after ack = %+v", f)
	}
	if out := run(t, "status"); !strings.Contains(out, "stack at risk from t1 up (acknowledged)") {
		t.Fatalf("status:\n%s", out)
	}

	out := run(t, "unstack", "#1")
	if !strings.Contains(out, "t1 is out of the PR stack") || !strings.Contains(out, "the stack checks clean") {
		t.Fatalf("unstack:\n%s", out)
	}
	es, err := a.Store.Train()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].State != app.TrainSuperseded {
		t.Fatalf("train = %+v, want t1 superseded", es)
	}
	if _, flagged, _ := a.Flag(); flagged {
		t.Fatal("flag outlived unstacking the only task")
	}
}
