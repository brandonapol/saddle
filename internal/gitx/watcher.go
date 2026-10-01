package gitx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// WatcherConfig configures a Watcher.
type WatcherConfig struct {
	// Integration is the ref ahead/behind is measured against, e.g. "saddle/integration".
	Integration string
	// Symbols extracts touched symbols; nil disables SymbolTouched deltas.
	Symbols SymbolExtractor
	// Debounce is the quiet period after the last filesystem event before a
	// worktree is re-read. Default 750ms.
	Debounce time.Duration
	// MaxWait bounds how long a stream of events can postpone a re-read.
	// Default 5s.
	MaxWait time.Duration
	// Buffer is the capacity of the Events channel. Default 256. When it is
	// full, re-reads block until the consumer catches up.
	Buffer int
	// OnError receives errors from re-reads and fsnotify. Nil drops them.
	OnError func(task string, err error)
}

// Watcher keeps the live state of a set of worktrees and emits Deltas as
// they change. It watches every directory of each worktree (minus ignored
// ones), the worktree's git dir (HEAD, index, rebase state) and the shared
// refs, then debounces bursts of events into one re-read per worktree.
type Watcher struct {
	cfg    WatcherConfig
	fsw    *fsnotify.Watcher
	events chan Delta
	cache  *symbolCache
	done   chan struct{}
	wg     sync.WaitGroup

	mu    sync.Mutex
	tasks map[string]*tracked
	once  sync.Once
}

type tracked struct {
	task, path, gitDir, commonDir, base string

	kick chan struct{}
	stop chan struct{}

	mu    sync.Mutex
	state State
	ready bool
}

// NewWatcher starts a watcher with no worktrees. Call Add for each one and
// Close when done.
func NewWatcher(cfg WatcherConfig) (*Watcher, error) {
	if cfg.Debounce <= 0 {
		cfg.Debounce = 750 * time.Millisecond
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 5 * time.Second
	}
	if cfg.MaxWait < cfg.Debounce {
		cfg.MaxWait = cfg.Debounce
	}
	if cfg.Buffer <= 0 {
		cfg.Buffer = 256
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		cfg:    cfg,
		fsw:    fsw,
		events: make(chan Delta, cfg.Buffer),
		cache:  newSymbolCache(),
		done:   make(chan struct{}),
		tasks:  map[string]*tracked{},
	}
	w.wg.Add(1)
	go w.dispatch()
	return w, nil
}

// Events delivers deltas from every watched worktree. It is closed by Close.
func (w *Watcher) Events() <-chan Delta { return w.events }

// Add starts watching the worktree at path under the name task. base is the
// ref the task was cut from; empty means the merge-base with the integration
// head. Add reads the state once and emits deltas for anything already
// dirty, renamed or touched (but not Committed: there is no previous HEAD).
func (w *Watcher) Add(task, path, base string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	gitDir, err := Run(path, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return err
	}
	commonDir, err := Run(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	t := &tracked{
		task: task, path: path, gitDir: gitDir, commonDir: commonDir, base: base,
		kick: make(chan struct{}, 1), stop: make(chan struct{}),
	}

	w.mu.Lock()
	if w.isClosed() {
		w.mu.Unlock()
		return errors.New("gitx: watcher closed")
	}
	if _, dup := w.tasks[task]; dup {
		w.mu.Unlock()
		return fmt.Errorf("gitx: %s is already watched", task)
	}
	w.tasks[task] = t
	w.mu.Unlock()

	if err := w.watchTree(t, path); err != nil {
		w.Remove(task)
		return err
	}
	for _, d := range []string{gitDir, commonDir} {
		if err := w.fsw.Add(d); err != nil {
			w.Remove(task)
			return err
		}
	}
	if err := w.watchDirs(filepath.Join(commonDir, "refs"), func(string) bool { return false }); err != nil {
		w.Remove(task)
		return err
	}

	w.refresh(t)
	w.wg.Add(1)
	go w.loop(t)
	return nil
}

// Remove stops watching a worktree. Directory watches shared with other
// worktrees (the common refs) stay.
func (w *Watcher) Remove(task string) {
	w.mu.Lock()
	t, ok := w.tasks[task]
	delete(w.tasks, task)
	var others []string
	for _, o := range w.tasks {
		others = append(others, o.path)
	}
	w.mu.Unlock()
	if !ok {
		return
	}
	close(t.stop)
outer:
	for _, p := range w.fsw.WatchList() {
		if !within(p, t.path) {
			continue
		}
		for _, o := range others {
			if within(p, o) {
				continue outer // a worktree nested inside this one
			}
		}
		_ = w.fsw.Remove(p)
	}
}

// State returns the last state read for task.
func (w *Watcher) State(task string) (State, bool) {
	w.mu.Lock()
	t, ok := w.tasks[task]
	w.mu.Unlock()
	if !ok {
		return State{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state, t.ready
}

// Refresh schedules a re-read of task as if a file had changed, e.g. after
// the integration branch moved in a way the watcher could not see.
func (w *Watcher) Refresh(task string) {
	w.mu.Lock()
	t, ok := w.tasks[task]
	w.mu.Unlock()
	if ok {
		t.poke()
	}
}

// Close stops the watcher and closes Events.
func (w *Watcher) Close() error {
	var err error
	w.once.Do(func() {
		w.mu.Lock()
		close(w.done)
		w.mu.Unlock()
		err = w.fsw.Close()
		w.wg.Wait()
		close(w.events)
	})
	return err
}

func (w *Watcher) isClosed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func (t *tracked) poke() {
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

// dispatch fans fsnotify events out to the worktrees they belong to.
func (w *Watcher) dispatch() {
	defer w.wg.Done()
	for {
		select {
		case <-w.done:
			return
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			w.report("", err)
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if ev.Op == fsnotify.Chmod {
				continue
			}
			w.route(ev)
		}
	}
}

func (w *Watcher) route(ev fsnotify.Event) {
	w.mu.Lock()
	var hit []*tracked
	for _, t := range w.tasks {
		if within(ev.Name, t.path) || within(ev.Name, t.gitDir) || within(ev.Name, t.commonDir) {
			hit = append(hit, t)
		}
	}
	w.mu.Unlock()

	if ev.Op&fsnotify.Create != 0 {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			for _, t := range hit {
				var err error
				if within(ev.Name, t.path) {
					err = w.watchTree(t, ev.Name)
				} else if within(ev.Name, filepath.Join(t.commonDir, "refs")) {
					err = w.watchDirs(ev.Name, func(string) bool { return false })
				}
				if err != nil {
					w.report(t.task, err)
				}
			}
		}
	}
	for _, t := range hit {
		t.poke()
	}
}

// loop debounces kicks for one worktree: a re-read runs once events have been
// quiet for Debounce, or MaxWait after the first event of a burst.
func (w *Watcher) loop(t *tracked) {
	defer w.wg.Done()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	var first time.Time
	pending := false
	for {
		select {
		case <-w.done:
			timer.Stop()
			return
		case <-t.stop:
			timer.Stop()
			return
		case <-t.kick:
			now := time.Now()
			if !pending {
				pending, first = true, now
			}
			wait := min(w.cfg.Debounce, w.cfg.MaxWait-now.Sub(first))
			timer.Reset(max(wait, 0))
		case <-timer.C:
			pending = false
			w.refresh(t)
		}
	}
}

// refresh re-reads one worktree and emits the deltas since the last read.
func (w *Watcher) refresh(t *tracked) {
	next, err := Snapshot(t.path, SnapshotOptions{
		Integration: w.cfg.Integration, Base: t.base, Symbols: w.cfg.Symbols, cache: w.cache,
	})
	if err != nil {
		w.report(t.task, err)
		return
	}
	next.Task = t.task

	t.mu.Lock()
	prev, ready := t.state, t.ready
	if next.Rebasing && ready {
		// Mid-rebase HEAD walks through intermediate commits. Hold the last
		// settled state and report one Rebased when the operation finishes.
		t.mu.Unlock()
		return
	}
	t.state, t.ready = next, true
	t.mu.Unlock()

	if !ready {
		prev = State{Head: next.Head}
	}
	for _, d := range diffStates(prev, next, func(a, b string) ([]string, bool) {
		if _, err := Run(t.path, "merge-base", "--is-ancestor", a, b); err != nil {
			return nil, false
		}
		out, err := Run(t.path, "rev-list", "--reverse", a+".."+b)
		if err != nil || out == "" {
			return nil, true
		}
		return strings.Split(out, "\n"), true
	}) {
		select {
		case w.events <- d:
		case <-w.done:
			return
		case <-t.stop:
			return
		}
	}
}

func (w *Watcher) report(task string, err error) {
	if w.cfg.OnError != nil {
		w.cfg.OnError(task, err)
	}
}

// watchTree watches root and every directory under it, skipping .git, nested
// repositories and directories git ignores.
func (w *Watcher) watchTree(t *tracked, root string) error {
	ignored := ignoredDirs(t.path)
	return w.watchDirs(root, func(p string) bool {
		if p == t.path {
			return false
		}
		if filepath.Base(p) == ".git" || ignored[p] {
			return true
		}
		_, err := os.Lstat(filepath.Join(p, ".git"))
		return err == nil // a nested repo or worktree, watched on its own
	})
}

func (w *Watcher) watchDirs(root string, skip func(string) bool) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // raced with a delete
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if skip(p) {
			return filepath.SkipDir
		}
		if err := w.fsw.Add(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}

// ignoredDirs lists directories git ignores in the worktree (absolute paths).
func ignoredDirs(dir string) map[string]bool {
	out, err := git(dir, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	m := map[string]bool{}
	if err != nil {
		return m
	}
	for _, p := range strings.Split(out, "\x00") {
		if strings.HasSuffix(p, "/") {
			m[filepath.Join(dir, p)] = true
		}
	}
	return m
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator))
}
