package app

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// UsageMeter tails the Claude Code transcript of every live task session
// (session ids come from the SessionStart hook) and persists the usage to the
// store as per-minute buckets. Only one process should run a meter. It is not
// safe for concurrent use; App.Usage is, so a UI can read while Run writes.
type UsageMeter struct {
	app *App
	// ClaudeDir is Claude Code's config dir. Empty means $CLAUDE_CONFIG_DIR,
	// then ~/.claude.
	ClaudeDir string
	sessions  map[string]*meterSession
}

type meterSession struct {
	task string
	col  *usage.Collector
	// synced is set once this process's view of the session has replaced
	// whatever an earlier process stored for it.
	synced  bool
	pending []usage.Bucket // collected but not yet written
}

// NewUsageMeter returns a meter with no sessions; Sync discovers them.
func (a *App) NewUsageMeter() *UsageMeter {
	return &UsageMeter{app: a, ClaudeDir: os.Getenv("CLAUDE_CONFIG_DIR"), sessions: map[string]*meterSession{}}
}

// Run syncs on every poll tick until ctx is done. Errors go to onErr, if set,
// and never stop the loop.
func (m *UsageMeter) Run(ctx context.Context, onErr func(error)) {
	every := m.app.Cfg.Usage.Poll
	if every <= 0 {
		every = config.Default().Usage.Poll
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := m.Sync(); err != nil && onErr != nil {
			onErr(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync picks up new and ended sessions, reads new transcript lines and writes
// the usage deltas.
//
// The first time a session is read, its whole transcript is aggregated and
// replaces the session's stored rows, so restarting the meter never counts a
// transcript twice. After that, each delta is upsert-added.
func (m *UsageMeter) Sync() error {
	ts, err := m.app.Store.Tasks()
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, t := range ts {
		if t.SessionID == "" || !t.Active() {
			continue
		}
		live[t.SessionID] = true
		s := m.sessions[t.SessionID]
		if s == nil {
			s = &meterSession{col: usage.NewCollector()}
			m.sessions[t.SessionID] = s
		}
		s.task = t.ID
		// Re-tracking keeps the cursor, and lets a transcript that appeared
		// somewhere other than the expected path be found later.
		s.col.Track(usage.Session{ID: t.SessionID, Task: t.ID, Agent: usage.Claude, Path: m.transcript(t.Worktree, t.SessionID)})
	}
	var errs []error
	for id, s := range m.sessions {
		bs, err := s.col.Poll()
		errs = append(errs, err)
		s.pending = append(s.pending, bs...)
		cur, _ := s.col.Cursor(id)
		// Until something has been read, a reset would wipe the stored rows
		// of a transcript that is merely missing.
		if s.synced || cur.Offset > 0 {
			if err := m.app.Store.AddUsage(id, !s.synced, s.pending); err != nil {
				errs = append(errs, err)
			} else {
				s.synced, s.pending = true, nil
			}
		}
		// Ended sessions get this final read, then are dropped.
		if !live[id] && len(s.pending) == 0 {
			delete(m.sessions, id)
		}
	}
	return errors.Join(errs...)
}

func (m *UsageMeter) transcript(cwd, session string) string {
	return agent.Claude{ConfigDir: m.ClaudeDir}.Usage().Transcript(cwd, session)
}

// UsageSummary is what the TUI shows: totals per model and per task over all
// recorded usage, and each configured trailing window against its cap.
type UsageSummary struct {
	Models  []store.UsageTotal
	Tasks   []store.UsageTotal
	Windows []UsageWindow
}

// UsageWindow is the usage in one trailing window.
type UsageWindow struct {
	Name string
	Span time.Duration
	Used int64 // tokens counted against the cap
	Cap  int64 // 0 means no cap configured
}

// HasBar reports whether the window has a cap to draw a bar against.
func (w UsageWindow) HasBar() bool { return w.Cap > 0 }

// Fraction is Used/Cap; it can exceed 1, and is 0 when there is no cap.
func (w UsageWindow) Fraction() float64 {
	if w.Cap <= 0 {
		return 0
	}
	return float64(w.Used) / float64(w.Cap)
}

// Usage summarizes stored usage, with windows ending at now.
func (a *App) Usage(now time.Time) (UsageSummary, error) {
	var u UsageSummary
	var err error
	if u.Models, err = a.Store.UsageByModel(time.Time{}, time.Time{}); err != nil {
		return u, err
	}
	if u.Tasks, err = a.Store.UsageByTask(time.Time{}, time.Time{}); err != nil {
		return u, err
	}
	for _, w := range a.Cfg.Usage.Windows {
		tok, err := a.Store.UsageSince(now.Add(-w.Span))
		if err != nil {
			return u, err
		}
		used := tok.Total()
		if !a.Cfg.Usage.CountCacheReads {
			used -= tok.CacheRead
		}
		u.Windows = append(u.Windows, UsageWindow{Name: w.Name, Span: w.Span, Used: used, Cap: w.Cap})
	}
	return u, nil
}
