package e2e

import (
	"os"
	"path/filepath"

	"github.com/brandonapol/saddle/internal/runq"
)

// isolateRunq points in-process saddle calls at this World's temp dirs.
// World.New redirects HOME and XDG_CONFIG_HOME, but the process otherwise
// keeps the host's XDG_STATE_HOME, XDG_CACHE_HOME and heavy-run settings
// (SADDLE_RUNQ, SADDLE_RUNQ_DB, SADDLE_RUNQ_LEASE). A host queue in enforce
// mode with a slot held then blocks a journey's in-process gate (#575).
//
// The user runq.toml stays at $XDG_CONFIG_HOME/saddle/runq.toml under the
// world's home. Host overrides that name a mode, a queue file, a lease or
// a proc fixture are cleared, so that file is what decides.
func (w *World) isolateRunq() {
	w.T.Helper()
	state := filepath.Join(w.Root, "xdg", "state")
	cache := filepath.Join(w.Root, "xdg", "cache")
	config := filepath.Join(w.Home, ".config")
	must(w.T, os.MkdirAll(state, 0o755))
	must(w.T, os.MkdirAll(cache, 0o755))
	must(w.T, os.MkdirAll(filepath.Join(config, "saddle"), 0o755))
	for _, kv := range [][2]string{
		{"XDG_STATE_HOME", state},
		{"XDG_CACHE_HOME", cache},
		{"XDG_CONFIG_HOME", config},
		{runq.EnvPath, filepath.Join(state, "saddle", "runq.db")},
		{runq.EnvBypass, ""},
		{runq.EnvLease, ""},
		{runq.EnvProcRoot, ""},
	} {
		w.T.Setenv(kv[0], kv[1])
	}
}
