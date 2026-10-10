// Package app is saddle's core: spawning agents, claims, the merge train and
// notices. The CLI, hooks and MCP server are thin layers over it.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/refguard"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
	"github.com/brandonapol/saddle/internal/usage"
)

const OrchestratorID = "t0"

type App struct {
	Root  string
	Cfg   config.Config
	Store *store.Store
	Tmux  tmux.Driver
	Bin   string
	// AdapterStatus lists which adapters can run here (#182); nil means
	// agent.Availability with the configured commands.
	AdapterStatus func() []agent.Status
	// OwnerNotify tells the owner about adapter rotation outside saddle
	// (#321); nil means the [notify] desktop and webhook settings.
	OwnerNotify func(text string)

	wake          wakeState // idle notice wake-ups (#183)
	previewLayout bool      // compute layout with Git objects, without worktree refs or hooks
}

// Open finds the repo from dir (SADDLE_ROOT wins) and opens its state.
func Open(dir string) (*App, error) {
	root := os.Getenv("SADDLE_ROOT")
	if root == "" {
		var err error
		if root, err = gitx.Root(dir); err != nil {
			return nil, fmt.Errorf("not in a git repo: %w", err)
		}
	}
	cfg, err := config.Load(root)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(root, ".saddle", "state.db"))
	if err != nil {
		return nil, err
	}
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if b, err := filepath.EvalSymlinks(bin); err == nil {
		bin = b
	}
	return &App{Root: root, Cfg: cfg, Store: st, Tmux: tmux.Tmux{Session: cfg.Session}, Bin: bin}, nil
}

func (a *App) Close() error { return a.Store.Close() }

func (a *App) stateDir(parts ...string) string {
	return filepath.Join(append([]string{a.Root, ".saddle"}, parts...)...)
}

// Init creates .saddle/, a config template and a git exclude entry, and
// installs the ref guard hook. It fails when the repo already has a
// reference-transaction hook saddle didn't write.
func (a *App) Init() error {
	if err := os.MkdirAll(a.stateDir("worktrees"), 0o755); err != nil {
		return err
	}
	cfgPath := a.stateDir("config.toml")
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(cfgPath, []byte(config.Template), 0o644); err != nil {
			return err
		}
	}
	if err := a.detectTestCmd(); err != nil {
		return err
	}
	if hookable(a.Bin) {
		if err := refguard.Install(a.Root, a.Bin); err != nil {
			return err
		}
	}
	exclude := filepath.Join(a.Root, ".git", "info", "exclude")
	b, _ := os.ReadFile(exclude)
	if !strings.Contains(string(b), "/.saddle/") {
		if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(exclude, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.WriteString("/.saddle/\n"); err != nil {
			return err
		}
	}
	return nil
}

// TestRefguardEnv opts a Go test binary in as the ref guard hook. Its TestMain
// must set it and answer `<bin> refguard <state>` with refguard.Hook.
const TestRefguardEnv = "SADDLE_TEST_REFGUARD"

// hookable reports whether bin can run as the ref guard hook. A Go test binary
// would rerun its whole suite on every ref update unless it opted in.
func hookable(bin string) bool {
	return !strings.HasSuffix(filepath.Base(bin), ".test") || os.Getenv(TestRefguardEnv) == "1"
}

// ensureIntegration fetches base and creates the integration branch from
// <remote>/<base> on first use. Later it reports when integration falls behind.
func (a *App) ensureIntegration() error {
	base := a.freshBase()
	if gitx.BranchExists(a.Root, a.Cfg.Integration) {
		a.checkBehind(base)
		return nil
	}
	_, err := gitx.Run(a.Root, "branch", "--no-track", a.Cfg.Integration, base)
	return err
}

type SpawnReq struct {
	ID     string
	Title  string
	Prompt string
	Claims []string
	Model  string
	Parent string
	Base   string // defaults to the integration branch
	Issue  int    // GitHub issue the task implements; its PR will close it
	// After lists tasks whose unmerged work this one builds on. Its PR
	// stacks on theirs even when their files don't overlap (#193).
	After []string
	// Adapter is the agent to launch: claude (the default), codex or grok.
	Adapter string
	Force   bool // ignore claim conflicts, the concurrency cap and paused launches
	// Confirm goes ahead when every claim covers work landed or queued tasks
	// already changed. Without it such a spawn returns ErrNeedsConfirm.
	Confirm bool
}

// ErrNeedsConfirm means a spawn owns no new work: every claim covers files
// that landed or queued tasks changed. That is usually a stack repair, which
// belongs to the owning task or restack, so the user must confirm it first.
var ErrNeedsConfirm = errors.New("spawn needs confirmation")

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

// activeWorkers counts live workers. Orphaned tasks (no window, no
// heartbeat) don't count: they must not block new spawns (#254).
func (a *App) activeWorkers() (int, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return 0, err
	}
	orphans, err := a.Orphans()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range ts {
		if t.Role == store.RoleWorker && (t.Status == store.Running || t.Status == store.Idle || t.Status == store.NeedsYou) && !orphans[t.ID] {
			n++
		}
	}
	return n, nil
}

// Spawn creates a task with its own branch and worktree and starts Claude in a new tmux window.
func (a *App) Spawn(r SpawnReq) (store.Task, error) {
	var t store.Task
	if strings.TrimSpace(r.Title) == "" {
		return t, errors.New("spawn: title is required")
	}
	// Availability is checked for a chosen adapter, or a session default
	// other than claude; the plain claude default spawns as it always has.
	checkAvail := r.Adapter != "" || a.Cfg.Harness != config.HarnessClaude
	if r.Adapter != "" {
		if _, err := agent.ByName(r.Adapter); err != nil {
			return t, err
		}
	}
	// No adapter means the session default (#150), rotated past adapters out
	// of quota (#321).
	picked, rotated, err := a.spawnAdapter(r.Adapter, r.Force)
	if err != nil {
		return t, err
	}
	r.Adapter = picked
	ad, err := agent.ByName(r.Adapter)
	if err != nil {
		return t, err
	}
	if !r.Force {
		if checkAvail {
			if err := a.checkAdapter(ad.Name()); err != nil {
				return t, err
			}
		}
		n, err := a.activeWorkers()
		if err != nil {
			return t, err
		}
		if limit := a.ConcurrencyLimit(); n >= limit {
			return t, concurrencyErr(n, limit)
		}
		if err := a.checkLaunch(); err != nil {
			return t, err
		}
		if err := a.checkSpawnCaps(r.Parent); err != nil {
			return t, err
		}
	}
	for i, c := range r.Claims {
		r.Claims[i] = claims.Clean(c)
	}
	if err := a.Init(); err != nil {
		return t, err
	}
	if err := a.ensureIntegration(); err != nil {
		return t, err
	}
	// Claims are checked before the row exists, so a conflict leaves nothing behind.
	if len(r.Claims) > 0 && !r.Force {
		all, err := a.Store.Claims()
		if err != nil {
			return t, err
		}
		if c := claims.Conflicts(all, "", r.Claims); len(c) > 0 {
			return t, conflictErr(c)
		}
	}
	if len(r.Claims) > 0 && !r.Force && !r.Confirm {
		if err := a.checkNewWork(r.Claims); err != nil {
			return t, err
		}
	}
	id := r.ID
	if id == "" {
		var err error
		if id, err = a.Store.NextID(); err != nil {
			return t, err
		}
	}
	if _, err := a.Store.Task(id); err == nil {
		return t, fmt.Errorf("task %s already exists", id)
	}
	after, err := a.cleanAfter(id, r.After)
	if err != nil {
		return t, err
	}
	name := id + "-" + slug(r.Title)
	base := r.Base
	if base == "" {
		base = a.Cfg.Integration
	}
	// A model from another adapter's family (a ticket's "opus", a repair's
	// "sonnet") falls back to this adapter's default.
	model := a.modelFor(ad.Name(), r.Model)
	hint := a.retryHint(r.Title)
	t = store.Task{
		ID: id, Title: r.Title, Prompt: r.Prompt, Parent: r.Parent, Role: store.RoleWorker, Model: model,
		Branch: "saddle/" + name, Worktree: a.stateDir("worktrees", name), Status: store.Running, Issue: r.Issue,
	}
	hadBranch := gitx.BranchExists(a.Root, t.Branch)
	if err := a.Store.CreateTask(t); err != nil {
		return t, err
	}
	// Claims go in before the worktree exists so a racing spawn sees them.
	if len(r.Claims) > 0 {
		_, err := a.Store.Claim(id, func(all map[string][]string) ([]string, error) {
			if c := claims.Conflicts(all, id, r.Claims); len(c) > 0 && !r.Force {
				return nil, conflictErr(c)
			}
			return r.Claims, nil
		})
		if err != nil {
			return t, a.spawnFailed(t, false, hadBranch, err)
		}
	}
	if err := a.setAfter(id, after); err != nil {
		return t, a.spawnFailed(t, false, hadBranch, err)
	}
	if err := gitx.WorktreeAdd(a.Root, t.Worktree, t.Branch, base); err != nil {
		return t, a.spawnFailed(t, false, hadBranch, fmt.Errorf("worktree add: %w", err))
	}
	win, err := a.launch(t, r.Claims, ad)
	if err != nil {
		return t, a.spawnFailed(t, true, hadBranch, fmt.Errorf("launch: %w", err))
	}
	t.Window = win
	a.Store.Event(id, "spawn", fmt.Sprintf("parent=%s adapter=%s model=%s claims=%s%s", r.Parent, ad.Name(), model, strings.Join(r.Claims, ","), hint))
	if rotated != "" {
		if err := a.rotatedTask(t, rotated, ad, r.Claims); err != nil {
			return t, err
		}
	}
	if r.Parent != "" && r.Parent != OrchestratorID {
		if err := a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s spawned sub-task %s %q.", r.Parent, id, r.Title)); err != nil {
			return t, err
		}
	}
	return t, nil
}

// checkNewWork returns ErrNeedsConfirm when every claim covers a file that a
// landed or queued task changed.
func (a *App) checkNewWork(want []string) error {
	changed := a.trainFiles()
	var lines []string
	for _, c := range want {
		hit := ""
		for f := range changed {
			if claims.Match(c, f) && (hit == "" || f < hit) {
				hit = f
			}
		}
		if hit == "" {
			return nil
		}
		lines = append(lines, fmt.Sprintf("%s covers %s (changed by %s)", c, hit, changed[hit]))
	}
	return fmt.Errorf("%w: every claim covers work landed or queued tasks already did: %s. "+
		"A task with no new work of its own is usually a stack repair: message the owning task, or call restack if the base moved or the stack is flagged at risk; never fix it with git. "+
		"Ask the user before spawning it anyway with confirm",
		ErrNeedsConfirm, strings.Join(lines, "; "))
}

// trainFiles maps each file a landed or queued task changed to that task.
func (a *App) trainFiles() map[string]string {
	out := map[string]string{}
	entries, err := a.Store.Train()
	if err != nil {
		return out
	}
	for _, e := range entries {
		t, err := a.Store.Task(e.Task)
		if err != nil || t.Status == store.Killed {
			continue
		}
		var files []string
		if e.State == store.TrainOK {
			from, to, ok := strings.Cut(e.Note, "..")
			if !ok {
				continue // recorded before the train kept ranges
			}
			files, _ = gitx.ChangedFiles(a.Root, from, to)
		} else if diff, err := gitx.Run(a.Root, "diff", "--name-only", a.Cfg.Integration+"..."+t.Branch); err == nil && diff != "" {
			files = strings.Split(diff, "\n")
		}
		for _, f := range files {
			out[f] = t.ID
		}
	}
	return out
}

func conflictErr(c map[string]string) error {
	var lines []string
	for want, owner := range c {
		lines = append(lines, fmt.Sprintf("%s overlaps %s", want, owner))
	}
	sort.Strings(lines)
	return fmt.Errorf("claim conflict: %s", strings.Join(lines, "; "))
}

func (a *App) launch(t store.Task, cl []string, ad agent.Adapter) (string, error) {
	return a.start(t, cl, ad, "", t.Prompt)
}

// start opens t's agent in a new window of the tmux session, creating the
// session (and the server) when it is gone. resume, if set, is the Claude
// session to continue; prompt is the first message.
func (a *App) start(t store.Task, cl []string, ad agent.Adapter, resume, prompt string) (string, error) {
	cmdName, args := a.adapterCmd(ad.Name())
	l := agent.Launch{
		Root: a.Root, Bin: a.Bin, Task: t.ID, Title: t.Title, Dir: t.Worktree, Model: t.Model,
		Mode: a.Cfg.Claude.PermissionMode, Cmd: cmdName, Args: args, RunDir: a.stateDir("run", t.ID),
		Brief: a.workerBrief(t, cl), Prompt: prompt, Resume: resume, ShimDir: a.WriteShims(),
	}
	// Grok workers run the full Grok CLI harness (hooks, MCP, tmux) with the
	// [grok] settings (#181). [adapters.grok] cmd and args still apply.
	if ad.Name() == usage.Grok {
		l.Kind = agent.KindGrok
		if l.Cmd == "" {
			l.Cmd = a.Cfg.Grok.Cmd
		}
		l.Mode = a.Cfg.Grok.PermissionMode
	}
	cmd, err := ad.Launch(l)
	if err != nil {
		return "", err
	}
	var win string
	if a.Tmux.HasSession() {
		win, err = a.Tmux.NewWindow(t.ID+"-"+slug(t.Title), t.Worktree, cmd)
	} else {
		win, err = a.Tmux.NewSession(t.ID+"-"+slug(t.Title), t.Worktree, cmd)
	}
	if err != nil {
		return "", err
	}
	return win, a.Store.SetField(t.ID, "window", win)
}

// EnsureOrchestrator makes sure saddle is initialized, the integration branch
// exists and the orchestrator task exists, and returns that task. It starts
// no process: the TUI launches one, the Claude Code plugin is one.
func (a *App) EnsureOrchestrator() (store.Task, error) {
	if err := a.Init(); err != nil {
		return store.Task{}, err
	}
	if err := a.ensureIntegration(); err != nil {
		return store.Task{}, err
	}
	t, err := a.Store.Task(OrchestratorID)
	if errors.Is(err, store.ErrNotFound) {
		t = store.Task{ID: OrchestratorID, Title: "orchestrator", Role: store.RoleOrchestrator,
			Model: a.orchModel(), Worktree: a.Root, Status: store.Running}
		err = a.Store.CreateTask(t)
	}
	return t, err
}

// Orchestrator ensures the orchestrator task exists and returns the launch
// for its headless process (Claude Code, or grok-bridge under harness =
// "grok"), plus the session to resume (if any).
func (a *App) Orchestrator() (agent.Launch, string, error) {
	t, err := a.EnsureOrchestrator()
	if err != nil {
		return agent.Launch{}, "", err
	}
	// It runs headless; a window left from an old tmux-based run is not its own.
	if err := errors.Join(a.Store.SetStatus(t.ID, store.Running), a.Store.SetField(t.ID, "window", "")); err != nil {
		return agent.Launch{}, "", err
	}
	l := a.newLaunch(t, a.Root, a.orchModel(), a.orchestratorBrief(), "", agent.OrchestratorAllow(), agent.OrchestratorDeny())
	if err := a.applyAdvisor(&l); err != nil {
		return agent.Launch{}, "", err
	}
	// A session started on another agent can't be resumed here (#150). Its
	// id stays stored until the new session reports its own.
	resume := t.SessionID
	if agent.Recorded(l.RunDir) != a.orchestratorAgent() {
		resume = ""
	}
	return l, resume, nil
}

// newLaunch fills the CLI-specific fields from config. Workers pass nil allow
// so the Claude adapter uses its edit allow-list; grok uses its permission mode.
func (a *App) newLaunch(t store.Task, dir, model, brief, prompt string, allow, deny []string) agent.Launch {
	l := agent.Launch{
		Root: a.Root, Bin: a.Bin, Task: t.ID, Title: t.Title, Dir: dir, Model: model,
		RunDir: a.stateDir("run", t.ID), Brief: brief, Prompt: prompt, Allow: allow, Deny: deny,
		ShimDir: a.WriteShims(),
	}
	if a.Cfg.Harness == config.HarnessGrok {
		l.Kind = agent.KindGrok
		l.Cmd, l.Args = a.adapterCmd(usage.Grok)
		if l.Cmd == "" {
			l.Cmd = a.Cfg.Grok.Cmd
		}
		l.Mode = a.Cfg.Grok.PermissionMode
		return l
	}
	l.Kind = agent.KindClaude
	l.Cmd = a.Cfg.Claude.Cmd
	l.Mode = a.Cfg.Claude.PermissionMode
	return l
}

func (a *App) workerModel() string {
	switch a.Cfg.Harness {
	case config.HarnessGrok:
		return a.Cfg.Grok.Model
	case config.HarnessCodex:
		return "" // codex's own default
	}
	return a.Cfg.Claude.Model
}

func (a *App) orchModel() string {
	if a.Cfg.Harness == config.HarnessGrok {
		return a.Cfg.Grok.OrchestratorModel
	}
	return a.Cfg.Claude.OrchestratorModel
}

// Down stops every agent (the tmux session). Live tasks are paused, not
// killed: claims, worktrees and branches stay, uncommitted work is
// snapshotted, and saddle up resumes them. It returns the paused tasks.
func (a *App) Down() ([]string, error) { return a.Pause() }

// Peek returns the last lines of a task's terminal.
func (a *App) Peek(task string, lines int) (string, error) {
	t, err := a.Store.Task(task)
	if err != nil {
		return "", err
	}
	if t.Window == "" || !a.Tmux.Alive(t.Window) {
		return "", fmt.Errorf("%s has no live window (status %s)", task, t.Status)
	}
	if lines <= 0 {
		lines = 40
	}
	out, err := a.Tmux.Capture(t.Window, lines)
	return strings.TrimRight(out, "\n "), err
}

// SendKeys presses keys in a task's terminal, e.g. "1" to pick a prompt option
// or "Escape". With text set, it types that text and presses Enter instead.
func (a *App) SendKeys(task, text string, keys []string) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if !a.ownWindow(t) {
		return fmt.Errorf("%s has no live window saddle opened for it", task)
	}
	a.Store.Event(task, "keys", text+strings.Join(keys, " "))
	if text != "" {
		return a.Tmux.SendText(t.Window, text)
	}
	return a.Tmux.SendKeys(t.Window, keys...)
}

// ClaimResult reports what a claim request got.
type ClaimResult struct {
	Granted []string          `json:"granted"`
	Denied  map[string]string `json:"denied,omitempty"`
}

// Claim grants every requested glob that no other active task overlaps.
func (a *App) Claim(task string, want []string) (ClaimResult, error) {
	res := ClaimResult{Denied: map[string]string{}}
	for i, w := range want {
		want[i] = claims.Clean(w)
	}
	granted, err := a.Store.Claim(task, func(all map[string][]string) ([]string, error) {
		c := claims.Conflicts(all, task, want)
		var g []string
		for _, w := range want {
			if owner, bad := c[w]; bad {
				res.Denied[w] = owner
			} else {
				g = append(g, w)
			}
		}
		return g, nil
	})
	res.Granted = granted
	if len(granted) > 0 {
		a.Store.Event(task, "claim", strings.Join(granted, ","))
	}
	return res, err
}

// Decision is the verdict on a write by an agent.
type Decision struct {
	Allow  bool
	Reason string
}

// CheckWrite decides whether task may write the file at abs. Paths nobody owns
// are claimed for the task on first write, so claims track real work.
func (a *App) CheckWrite(task, abs string) Decision {
	t, err := a.Store.Task(task)
	if err != nil || t.Role != store.RoleWorker || !t.Active() {
		return Decision{Allow: true}
	}
	abs = filepath.Clean(abs)
	wt := filepath.Clean(t.Worktree)
	rel, err := filepath.Rel(wt, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		if r, err := filepath.Rel(a.Root, abs); err == nil && !strings.HasPrefix(r, "..") {
			return Decision{Reason: fmt.Sprintf("%s is outside your worktree. You work in %s on branch %s; edit your copy of the file there instead.", abs, wt, t.Branch)}
		}
		return Decision{Allow: true} // outside the repo entirely (scratch files etc.)
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, ".git/") {
		return Decision{Allow: true}
	}
	for _, g := range a.Cfg.Serial {
		if claims.Match(g, rel) {
			mine, _ := a.Store.Claims()
			for _, own := range mine[task] {
				if claims.Match(own, rel) {
					return Decision{Allow: true}
				}
			}
			return Decision{Reason: fmt.Sprintf("%s is a serial file (%s) owned by the merge train. Don't edit it; it is regenerated when your branch lands. If you truly need a change, say so in your done summary.", rel, g)}
		}
	}
	var reason string
	_, err = a.Store.Claim(task, func(all map[string][]string) ([]string, error) {
		for _, own := range all[task] {
			if claims.Match(own, rel) {
				return nil, nil
			}
		}
		if owner, glob := claims.Owner(all, task, rel); owner != "" {
			ot, _ := a.Store.Task(owner)
			reason = fmt.Sprintf("%s is claimed by %s (%q, claim %s). Don't edit it: work around it (depend on an interface, stub it in tests), or finish your part and mention the needed change in your done summary. The orchestrator can sequence the work.", rel, owner, ot.Title, glob)
			return nil, nil
		}
		return []string{rel}, nil
	})
	if err != nil {
		return Decision{Allow: true} // fail open
	}
	if reason != "" {
		a.Store.Event(task, "deny", rel)
		return Decision{Reason: reason}
	}
	return Decision{Allow: true}
}

// Notify queues a notice for a task. Action notices wake an idle worker by
// typing into its window; info notices arrive with its next tool call. The
// orchestrator is never typed at: the TUI delivers its notices.
func (a *App) Notify(task, kind, text string) error {
	if !a.admitNotice(task, kind, text) {
		return nil
	}
	if err := a.Store.Notify(task, kind, text); err != nil {
		return err
	}
	if kind != store.NoticeAction {
		return nil
	}
	t, err := a.Store.Task(task)
	if err != nil || t.Window == "" || !t.Active() {
		return nil
	}
	if ad := a.taskAdapter(t); !ad.Hooks() {
		return a.injectNotices(t, ad, false)
	}
	// A needs-you agent is woken only when its screen shows no prompt: an
	// escalated task waits idle at its prompt for instructions (#189), while
	// the wake line would answer a permission prompt.
	if waitsAtPrompt(t) && a.ownWindow(t) {
		if t.Status == store.NeedsYou && a.onPrompt(t) {
			return nil
		}
		a.sendWake(t, false)
	}
	return nil
}

// onPrompt reports whether t's window shows a permission prompt or question,
// or can't be read.
func (a *App) onPrompt(t store.Task) bool {
	screen, err := a.Tmux.Capture(t.Window, 30)
	return err != nil || DetectPrompt(screen) != PromptNone
}

// Done marks a task finished and queues its branch in the merge train.
func (a *App) Done(task, summary string) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if t.Role != store.RoleWorker {
		return errors.New("only worker tasks can be done")
	}
	dirty, err := gitx.Dirty(t.Worktree)
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf("worktree has uncommitted changes; commit them first:\n%s", strings.Join(dirty, "\n"))
	}
	n, err := gitx.CommitsBetween(t.Worktree, a.Cfg.Integration, "HEAD")
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("branch has no commits beyond the integration branch; nothing to land")
	}
	if err := a.lintDone(t); err != nil {
		return err
	}
	if summary != "" {
		if err := a.Store.SetField(task, "summary", summary); err != nil {
			return err
		}
	}
	if err := a.Store.SetStatus(task, store.Done); err != nil {
		return err
	}
	if err := a.Store.Enqueue(task); err != nil {
		return err
	}
	a.Store.Event(task, "done", summary)
	advisory := ""
	if !a.taskAdapter(t).Hooks() {
		advisory = a.advisoryClaims(t)
	}
	return a.Notify(OrchestratorID, store.NoticeAction,
		fmt.Sprintf("%s %q is done and queued in the merge train: %s\nRun the saddle land tool when you're ready.%s", task, t.Title, summary, advisory))
}

// Kill stops a task's window and releases its claims. Unless keep is set it
// removes the worktree, and the branch too when it has no commits beyond
// integration. A branch with commits, or a worktree with uncommitted
// changes, is always kept.
func (a *App) Kill(task string, keep bool) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if t.Role == store.RoleWorker {
		a.snapshotWIP(t.ID, t.Worktree)
		a.pruneCheckpoint(t.ID) // the wip snapshot holds the latest work (#50)
	}
	// Dead before its window goes, so the resume watcher won't bring it back.
	// The ref guard won't delete a live task's branch either.
	if err := errors.Join(a.Store.Release(task), a.Store.SetStatus(task, store.Killed)); err != nil {
		return err
	}
	if t.Window != "" && a.Tmux.Alive(t.Window) {
		if err := a.Tmux.KillWindow(t.Window); err != nil {
			return err
		}
	}
	note := ""
	if !keep && t.Role == store.RoleWorker {
		if note, err = a.cleanup(t); err != nil {
			return err
		}
	}
	a.Store.Event(task, "kill", note)
	return nil
}

// TaskForDir finds the worker task whose worktree contains dir.
func (a *App) TaskForDir(dir string) (store.Task, error) {
	top, err := gitx.Toplevel(dir)
	if err != nil {
		return store.Task{}, err
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return store.Task{}, err
	}
	for _, t := range ts {
		if t.Role == store.RoleWorker && filepath.Clean(t.Worktree) == filepath.Clean(top) {
			return t, nil
		}
	}
	return store.Task{}, fmt.Errorf("%s is not a saddle task worktree", top)
}
