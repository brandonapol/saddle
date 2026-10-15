package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// Status carries forecast only while a pair of live tasks scores medium or
// above; brief lists the pairs that involve the briefed task.
func TestStatusAndBriefForecast(t *testing.T) {
	a, _ := stackSetup(t)
	var tasks []string
	for _, id := range []string{"t2", "t3", "t4"} {
		tk, err := a.Spawn(app.SpawnReq{ID: id, Title: id})
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, tk.Worktree)
	}

	out := callStatus(t, a)
	if out.Forecast != nil {
		t.Fatalf("forecast with no overlapping work: %+v", out.Forecast)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), `"forecast"`) {
		t.Fatalf("status JSON carries forecast with no medium+ pairs: %s", b)
	}

	write(t, tasks[0], "README.md", "t2 was here\n")
	write(t, tasks[1], "README.md", "t3 was here\n")
	out = callStatus(t, a)
	f := out.Forecast
	if f == nil || len(f.Pairs) != 1 {
		t.Fatalf("forecast = %+v, want one pair", f)
	}
	if p := f.Pairs[0]; p.A != "t2" || p.B != "t3" || p.Level != "high" || p.Reason != "same file: README.md" {
		t.Fatalf("pair = %+v", p)
	}
	if f.Safe != 2 {
		t.Fatalf("safe = %d, want 2 (t2/t4, t3/t4)", f.Safe)
	}

	var br BriefOut
	if res := callTool(t, a, "t3", "brief", map[string]any{}, &br); res.IsError {
		t.Fatalf("brief failed: %s", errText(res))
	}
	if len(br.Forecast) != 1 || br.Forecast[0].A != "t2" || br.Forecast[0].Level != "high" {
		t.Fatalf("brief forecast = %+v", br.Forecast)
	}
	br = BriefOut{}
	if res := callTool(t, a, "t4", "brief", map[string]any{}, &br); res.IsError || len(br.Forecast) != 0 {
		t.Fatalf("t4 brief forecast = %+v", br.Forecast)
	}
}
