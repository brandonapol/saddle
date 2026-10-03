package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The concurrency cap (#176): config's concurrency, overridden at runtime by
// `saddle concurrency N`, the MCP tool or the TUI plan view. The override is
// a file under .saddle/, like the auto-merge toggle, so it survives restarts
// without touching the store schema.

const (
	// MinConcurrency and MaxConcurrency bound a runtime override.
	MinConcurrency = 1
	MaxConcurrency = 16

	// EventConcurrency is logged against the orchestrator when the cap changes.
	EventConcurrency = "concurrency"

	ConcurrencyFromConfig = "config"
	ConcurrencyRuntime    = "runtime"
)

// Concurrency is the worker cap in force and how many workers count against it.
type Concurrency struct {
	Limit   int    `json:"limit"`
	Running int    `json:"running"`
	Source  string `json:"source"` // config or runtime
	Config  int    `json:"config"` // the configured value, which reset returns to
}

type concurrencyFile struct {
	Limit int `json:"limit"`
}

func (a *App) concurrencyPath() string { return a.stateDir("concurrency.json") }

// concurrencyOverride is the saved runtime limit, or 0 when there is none or
// it can't be read.
func (a *App) concurrencyOverride() int {
	b, err := os.ReadFile(a.concurrencyPath())
	if err != nil {
		return 0
	}
	var f concurrencyFile
	if json.Unmarshal(b, &f) != nil || f.Limit < MinConcurrency || f.Limit > MaxConcurrency {
		return 0
	}
	return f.Limit
}

// ConcurrencyLimit is how many workers may run at once: the runtime override
// if set, else config's concurrency.
func (a *App) ConcurrencyLimit() int {
	if n := a.concurrencyOverride(); n > 0 {
		return n
	}
	return a.Cfg.Concurrency
}

// Concurrency reports the cap in force and how many workers are running.
func (a *App) Concurrency() (Concurrency, error) {
	c := Concurrency{Limit: a.Cfg.Concurrency, Source: ConcurrencyFromConfig, Config: a.Cfg.Concurrency}
	if n := a.concurrencyOverride(); n > 0 {
		c.Limit, c.Source = n, ConcurrencyRuntime
	}
	var err error
	c.Running, err = a.activeWorkers()
	return c, err
}

// SetConcurrency overrides the cap at runtime. Spawn honors it at once;
// lowering it stops nothing that runs, it only holds new spawns until the
// running count is under it.
func (a *App) SetConcurrency(n int) (Concurrency, error) {
	if n < MinConcurrency || n > MaxConcurrency {
		return Concurrency{}, fmt.Errorf("concurrency %d: want %d to %d", n, MinConcurrency, MaxConcurrency)
	}
	old := a.ConcurrencyLimit()
	b, _ := json.Marshal(concurrencyFile{Limit: n})
	if err := writeFileAtomic(a.concurrencyPath(), append(b, '\n')); err != nil {
		return Concurrency{}, err
	}
	c, err := a.Concurrency()
	if err != nil {
		return c, err
	}
	a.logConcurrency(old, c)
	return c, nil
}

// ResetConcurrency drops the runtime override, back to config's value.
func (a *App) ResetConcurrency() (Concurrency, error) {
	old := a.ConcurrencyLimit()
	if err := os.Remove(a.concurrencyPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Concurrency{}, err
	}
	c, err := a.Concurrency()
	if err != nil {
		return c, err
	}
	a.logConcurrency(old, c)
	return c, nil
}

func (a *App) logConcurrency(old int, c Concurrency) {
	if old == c.Limit {
		return
	}
	msg := fmt.Sprintf("concurrency %d → %d (%s, %d running)", old, c.Limit, c.Source, c.Running)
	if c.Running > c.Limit {
		msg += "; nothing is stopped, new spawns wait until fewer run"
	}
	a.Store.Event(OrchestratorID, EventConcurrency, msg)
}

// concurrencyErr is spawn's refusal at the cap.
func concurrencyErr(running, limit int) error {
	return fmt.Errorf("at concurrency cap (%d running, limit %d): nothing was stopped; new spawns wait until fewer than %d run. Wait for a task to finish, or raise the limit with `saddle concurrency N` (the concurrency tool)", running, limit, limit)
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
