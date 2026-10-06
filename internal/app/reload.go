package app

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// trainSections are the parts of the config the merge train acts on. A
// long-lived saddle (up, mcp) re-reads them before every land, so an edit
// to .saddle/config.toml takes effect without a restart (#228).
type trainSections struct {
	Test         config.Test
	Lint         config.Lint
	MaxAttempts  int
	NoAutoRebase bool
	Regen        []config.Regen
}

func sectionsOf(c config.Config) trainSections {
	return trainSections{Test: c.Test, Lint: c.Train.Lint, MaxAttempts: c.Train.MaxAttempts,
		NoAutoRebase: c.Train.NoAutoRebase, Regen: c.Regen}
}

// processStart is when this saddle started. A config file older than that
// was read at Open as it is now.
var processStart = time.Now()

// seenSections is what each App last read from its config files.
var seenSections = struct {
	sync.Mutex
	m map[*App]trainSections
}{m: map[*App]trainSections{}}

// ReloadTrainConfig re-reads the train's config sections ([test], [train]
// lint.cmd, max_attempts, no_auto_rebase, [[regen]]) from the config files
// and applies the ones that changed since saddle last read them. Only
// changes are applied, so a setting made in memory (by a test, say) stands
// until the file changes it. It reports whether anything changed.
func (a *App) ReloadTrainConfig() (bool, error) {
	cfg, err := config.Load(a.Root)
	if err != nil {
		return false, err
	}
	now := sectionsOf(cfg)
	seenSections.Lock()
	defer seenSections.Unlock()
	seen, ok := seenSections.m[a]
	if !ok {
		// First reload. A file untouched since saddle started is what Open
		// read; one edited since may hold changes Open never saw, so every
		// value it sets counts as a change.
		seen = now
		if a.configChangedSince(processStart) {
			// What saddle reads with no project file at all.
			bare, err := config.Load(filepath.Join(a.Root, ".saddle", "no-project-config"))
			if err != nil {
				return false, err
			}
			seen = sectionsOf(bare)
		}
	}
	seenSections.m[a] = now
	changed := false
	apply := func(differs bool, set func()) {
		if differs {
			set()
			changed = true
		}
	}
	apply(!reflect.DeepEqual(now.Test, seen.Test), func() { a.Cfg.Test = now.Test })
	apply(now.Lint != seen.Lint, func() { a.Cfg.Train.Lint = now.Lint })
	apply(now.MaxAttempts != seen.MaxAttempts, func() { a.Cfg.Train.MaxAttempts = now.MaxAttempts })
	apply(now.NoAutoRebase != seen.NoAutoRebase, func() { a.Cfg.Train.NoAutoRebase = now.NoAutoRebase })
	apply(!reflect.DeepEqual(now.Regen, seen.Regen), func() { a.Cfg.Regen = now.Regen })
	return changed, nil
}

// configChangedSince reports whether a config file saddle reads was
// modified after t.
func (a *App) configChangedSince(t time.Time) bool {
	paths := []string{filepath.Join(a.Root, ".saddle", "config.toml")}
	if home, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(home, "saddle", "config.toml"))
	}
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(t) {
			return true
		}
	}
	return false
}

// ReloadOnSIGHUP re-reads the train's config sections each time the process
// gets SIGHUP, until ctx is done. Only for processes without a terminal
// (saddle mcp): to saddle up, SIGHUP means its terminal closed.
func (a *App) ReloadOnSIGHUP(ctx context.Context) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	a.reloadOn(ctx, ch)
}

func (a *App) reloadOn(ctx context.Context, ch <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if _, err := a.ReloadTrainConfig(); err != nil {
				a.Store.Event("", "config_reload_failed", err.Error())
			} else {
				a.Store.Event("", "config_reloaded", "SIGHUP")
			}
		}
	}
}
