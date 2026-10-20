// Package app is saddle's core: spawning agents, claims, the merge train and
// notices. The CLI, hooks and MCP server are thin layers over it.
package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

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

	wake          wakeState        // idle notice wake-ups (#183)
	doneJobs      sync.WaitGroup   // done gates running in the background (#368)
	runReap       func(dir string) // tests stand in for reaping a run dir's processes
	previewLayout bool             // compute layout with Git objects, without worktree refs or hooks
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
func (a *App) ensureIntegration() error { return a.ensureIntegrationFetch(true) }

// ensureIntegrationFetch creates integration from a fresh base when it is
// missing. When it exists, fetch has it fetch base and warn when
// integration is behind; without fetch it is left alone.
func (a *App) ensureIntegrationFetch(fetch bool) error {
	exists := gitx.BranchExists(a.Root, a.Cfg.Integration)
	if exists && !fetch {
		return nil
	}
	base := a.freshBase()
	if exists {
		a.checkBehind(base)
		return nil
	}
	_, err := gitx.Run(a.Root, "branch", "--no-track", a.Cfg.Integration, base)
	return err
}

// CheckIntegrationBehind fetches base and tells the orchestrator when
// integration is behind it: the check Orchestrator skips, which saddle up
// runs after its first frame instead (#635).
func (a *App) CheckIntegrationBehind() { a.checkBehind(a.freshBase()) }

type SpawnReq struct {
	ID     string
	Title  string
	Prompt string
	Claims []string
	Model  string
	// ModelHint is a model a ticket mentions (an old "Model scope:" line):
	// used only when the chosen adapter runs it, and it never steers the
	// pick, unlike Model (#505).
	ModelHint string
	Parent    string
	Base      string // defaults to the integration branch
	Issue     int    // GitHub issue the task implements; its PR will close it
	// After lists tasks whose unmerged work this one builds on. Its PR
	// stacks on theirs even when their files don't overlap (#193).
	After []string
	// Adapter is the agent to launch: claude (the default), codex or grok.
	Adapter string
	// Kind is the task's kind (docs, mechanical, design…). Without Adapter
	// it picks an adapter whose [adapters.<name>] kinds list it (#481).
	Kind string
	// Difficulty is the ticket's difficulty (hard, standard, mechanical).
	// Without Adapter it prefers adapters whose capability table suits it,
	// and picks the model [adapters.<name>] models names for it (#505).
	Difficulty string
	// Needs are capabilities the task needs (review, image-gen,
	// hooks-required…). Without Adapter it goes to an adapter that has them
	// when one has quota (#505).
	Needs []string
	Force bool // ignore claim conflicts, the concurrency cap and paused launches
	// Confirm goes ahead when every claim covers work landed or queued tasks
	// already changed. Without it such a spawn returns ErrNeedsConfirm, unless
	// it carries an Issue: a ticket of its own is new work (#604).
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
	if err := repairOfRepairErr(r.Title); err != nil {
		return t, err
	}
	// Availability is checked for a chosen adapter, or a session default
	// other than claude; the plain claude default spawns as it always has.
	checkAvail := r.Adapter != "" || a.Cfg.Harness != config.HarnessClaude
	if r.Adapter != "" {
		if _, err := agent.ByName(r.Adapter); err != nil {
			return t, err
		}
	}
	// Excluded models are refused even with force: the owner never wants
	// workers on them (#482).
	if err := a.excludedModelErr(r.Model); err != nil {
		return t, err
	}
	// No adapter means the session default (#150), rotated past adapters out
	// of quota (#321).
	pick, err := a.spawnAdapter(r.Adapter, r.Model, a.spawnPickReq(r), r.Force)
	if err != nil {
		return t, err
	}
	r.Adapter = pick.Name
	rotated, why := pick.Rotated, pick.Why
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
			return t, a.conflictErr(c)
		}
	}
	if len(r.Claims) > 0 && !r.Force && !r.Confirm && r.Issue == 0 {
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
	model := a.spawnModel(pick, r)
	hint := a.retryHint(r.Title)
	t = store.Task{
		ID: id, Title: r.Title, Prompt: r.Prompt, Parent: r.Parent, Role: store.RoleWorker, Model: model, Adapter: ad.Name(),
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
	a.Store.Event(id, "spawn", fmt.Sprintf("parent=%s adapter=%s (%s) model=%s claims=%s%s", r.Parent, ad.Name(), why, model, strings.Join(r.Claims, ","), hint))
	a.Store.Event(id, EventAdapterPick, pickLine(ad.Name(), model, why, rotated))
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
		lines = append(lines, fmt.Sprintf("%s covers %s (changed by %s)", c, hit, a.ownerState(changed[hit])))
	}
	return fmt.Errorf("%w: every claim covers work landed or queued tasks already did: %s. "+
		"A task with no new work of its own is usually a stack repair. The owning task's agent is usually gone once its work landed or queued, so a message to it reaches no one (#340). "+
		"Call restack if the base moved or the stack is flagged at risk; for a repair restack can't do, call spawn again with confirm=true once the user agreed. Never fix it with git",
		ErrNeedsConfirm, strings.Join(lines, "; "))
}

// ownerState is "t3 (landed)": a task id with its status, so a refusal says
// whether the owner can still answer.
func (a *App) ownerState(id string) string {
	t, err := a.Store.Task(id)
	if err != nil {
		return id
	}
	return id + " (" + t.Status + ")"
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

// conflictErr is conflictErr with each owner's status and what to do when
// its agent is gone (#340).
func (a *App) conflictErr(c map[string]string) error {
	var lines, owners []string
	for want, owner := range c {
		id, glob, _ := strings.Cut(owner, " ")
		lines = append(lines, fmt.Sprintf("%s overlaps %s, claim %s", want, a.ownerState(id), strings.Trim(glob, "()")))
		if !slices.Contains(owners, id) {
			owners = append(owners, id)
		}
	}
	sort.Strings(lines)
	sort.Strings(owners)
	return fmt.Errorf("claim conflict: %s. "+
		"An owner that is still working can be messaged, or waited for until it lands. "+
		"When its agent is gone (stopped, orphaned or stuck), a message reaches no one: kill %s to release its claims and spawn again, "+
		"or spawn with claims no live task holds",
		strings.Join(lines, "; "), strings.Join(owners, ", kill "))
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
	cmd = a.taskTmpCmd(t.ID, cmd)
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
func (a *App) EnsureOrchestrator() (store.Task, error) { return a.ensureOrchestrator(true) }

// ensureOrchestrator is EnsureOrchestrator; fetch is ensureIntegrationFetch's.
func (a *App) ensureOrchestrator(fetch bool) (store.Task, error) {
	if err := a.Init(); err != nil {
		return store.Task{}, err
	}
	if err := a.ensureIntegrationFetch(fetch); err != nil {
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
// for its headless process (Claude Code, grok-bridge or codex-bridge: see
// orchDefault), plus the session to resume (if any). It doesn't fetch base when
// integration exists: saddle up runs CheckIntegrationBehind after its first
// frame (#635).
func (a *App) Orchestrator() (agent.Launch, string, error) {
	t, err := a.ensureOrchestrator(false)
	if err != nil {
		return agent.Launch{}, "", err
	}
	// It runs headless; a window left from an old tmux-based run is not its own.
	if err := errors.Join(a.Store.SetStatus(t.ID, store.Running), a.Store.SetField(t.ID, "window", "")); err != nil {
		return agent.Launch{}, "", err
	}
	a.clearStaleOverride(time.Now())
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

// newLaunch fills the orchestrator's CLI-specific fields from config. Workers pass nil allow
// so the Claude adapter uses its edit allow-list; grok uses its permission mode.
func (a *App) newLaunch(t store.Task, dir, model, brief, prompt string, allow, deny []string) agent.Launch {
	l := agent.Launch{
		Root: a.Root, Bin: a.Bin, Task: t.ID, Title: t.Title, Dir: dir, Model: model,
		RunDir: a.stateDir("run", t.ID), Brief: brief, Prompt: prompt, Allow: allow, Deny: deny,
		ShimDir: a.WriteShims(),
	}
	switch a.orchestratorAgent() {
	case usage.Grok:
		l.Kind = agent.KindGrok
		l.Cmd, l.Args = a.adapterCmd(usage.Grok)
		if l.Cmd == "" {
			l.Cmd = a.Cfg.Grok.Cmd
		}
		l.Mode = a.Cfg.Grok.PermissionMode
		return l
	case usage.Codex:
		// [orchestrator] adapter, a machine without claude (#668) or a
		// failover (#506) puts the orchestrator on codex; it runs headless
		// through the codex bridge.
		l.Kind = agent.KindCodex
		l.Cmd, l.Args = a.adapterCmd(usage.Codex)
		l.Mode = a.Cfg.Claude.PermissionMode
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
		return a.Cfg.Adapters[usage.Codex].Model // empty is codex's own default
	}
	return a.Cfg.Claude.Model
}

// orchModel is the orchestrator's model on the adapter it runs on (#668);
// empty is the CLI's own default.
func (a *App) orchModel() string { return agent.OrchestratorModel(a.Cfg, a.orchestratorAgent()) }

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
		// An Enter that didn't take leaves the text in the box (#467).
		ok, err := tmux.Submit(a.Tmux, t.Window, text)
		if err == nil && !ok {
			a.Store.Event(task, "keys_unsubmitted", text)
			return fmt.Errorf("typed into %s, but the text still sits in its input box after %d more Enters; peek at it", task, tmux.SubmitRetries)
		}
		return err
	}
	return a.Tmux.SendKeys(t.Window, keys...)
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

// promptShown reports whether a captured pane, styled or plain, shows a
// permission prompt or question, so nothing may be typed into it (#496).
func promptShown(pane string) bool {
	return tmux.ReadInput(pane) == tmux.InputPrompt || DetectPrompt(tmux.StripANSI(pane)) != PromptNone
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
	if err := errors.Join(a.Store.Release(task), a.Store.DequeuePending(task), a.Store.SetStatus(task, store.Killed)); err != nil {
		return err
	}
	if t.Window != "" && a.Tmux.Alive(t.Window) {
		if err := a.Tmux.KillWindow(t.Window); err != nil {
			return err
		}
	}
	a.removeTaskTmp(t.ID)
	a.cleanRunDir(t.ID)
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
