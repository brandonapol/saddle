package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPerfReportsReadsAndBacklog(t *testing.T) {
	a, _ := landedPR(t)
	t.Chdir(a.Root)

	var out strings.Builder
	cmd := Root()
	cmd.SetArgs([]string{"perf", "--runs", "2", "--json"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var r PerfReport
	if err := json.Unmarshal([]byte(out.String()), &r); err != nil {
		t.Fatalf("%v:\n%s", err, out.String())
	}
	if len(r.Reads) != 2 || r.Reads[0].Name != "tui tasks" || r.Reads[0].GitCalls != 0 {
		t.Fatalf("reads = %+v, want the tui read first and running no git", r.Reads)
	}
	if r.Tasks["landed"] != 1 || r.Rows["tasks"] == 0 || r.Rows["events"] == 0 {
		t.Fatalf("tasks = %v rows = %v, want the landed task and its events counted", r.Tasks, r.Rows)
	}

	out.Reset()
	cmd = Root()
	cmd.SetArgs([]string{"perf", "--runs", "1"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tui tasks", "status", "landed 1", "leftovers"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("perf output lacks %q:\n%s", want, out.String())
		}
	}
}
