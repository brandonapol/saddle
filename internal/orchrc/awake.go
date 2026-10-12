package orchrc

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/orchrc/inhibit"
)

// KeepAwake holds the platform's sleep inhibitor while a's autopilot is on
// and not paused, until ctx ends. Without one it logs that once and
// returns; saddle doctor warns about it too.
func KeepAwake(ctx context.Context, a *app.App) {
	logf := func(f string, args ...any) { a.Store.Event(app.OrchestratorID, "inhibit", fmt.Sprintf(f, args...)) }
	t, ok := inhibit.For(runtime.GOOS, exec.LookPath)
	if !ok {
		logf("sleep inhibitor: none found; %s", inhibit.Hint(runtime.GOOS))
		return
	}
	on := func() (bool, error) {
		st, err := a.AutopilotState()
		return st.On && !st.Paused, err
	}
	inhibit.Watch(ctx, t, on, inhibit.Exec, 30*time.Second, logf)
}
