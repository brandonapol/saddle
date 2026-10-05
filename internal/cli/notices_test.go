package cli

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #222: saddle notices prints the pending digest; --all lists every
// orchestrator notice with how it was routed.
func TestNoticesCommand(t *testing.T) {
	a := automergeRepo(t)
	t.Chdir(a.Root)
	if out := run(t, "notices"); !strings.Contains(out, "Nothing new") {
		t.Fatalf("empty digest:\n%s", out)
	}
	a.Store.Event(app.OrchestratorID, app.EventNoticeInterrupt, "t7 asks: per-user or global?")
	a.Store.Event(app.OrchestratorID, app.EventNoticeDigest, `[landed] t1 "alpha" landed on saddle/integration at 0123456789ab.`)
	a.Store.Event(app.OrchestratorID, app.EventNoticeSilent, "[repeat] The PR stack checks clean again.")
	if out := run(t, "notices"); !strings.Contains(out, "1 task landed (t1)") {
		t.Fatalf("digest:\n%s", out)
	}
	out := run(t, "notices", "--all")
	for _, want := range []string{"interrupt   t7 asks", "digest      [landed] t1", "silent      [repeat]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--all lacks %q:\n%s", want, out)
		}
	}
}
