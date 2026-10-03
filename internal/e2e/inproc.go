package e2e

import (
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/automerge"
)

// App opens the World's repo in this process, as the saddle binary would.
// gh calls go to the shim on PATH and tmux calls to the private server.
func (w *World) App() *app.App {
	w.T.Helper()
	a, err := app.Open(w.Repo)
	must(w.T, err)
	w.T.Cleanup(func() { _ = a.Close() })
	return a
}

// AutomergeTick runs one check of the auto-merge watcher, the loop `saddle
// up` and `saddle plugin engine` run every two minutes, with no wait.
func (w *World) AutomergeTick() automerge.Status {
	w.T.Helper()
	a, err := app.Open(w.Repo)
	must(w.T, err)
	defer func() { _ = a.Close() }()
	st, err := a.NewAutomerge(nil).Check()
	must(w.T, err)
	return st
}
