package bench

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/e2e"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/store"
)

// Adapters and granularities the runner knows. Only the first of each runs
// today; the rest are reserved (docs/bench.md, "Not yet").
var (
	Adapters      = []string{"fake", "claude", "grok", "codex"}
	Granularities = []string{"file", "symbol"}
)

// Defaults for Options' zero fields.
const (
	DefaultStall   = 60 * time.Second
	DefaultTimeout = 10 * time.Minute
)

// release is the message that lets a held fake agent call done.
const release = "bench-release"

// Options configure one run.
type Options struct {
	BenchDir    string // the repo's bench/ directory
	Scenario    string
	Adapter     string // "" means fake
	Granularity string // "" means file
	// Out is where the JSON and markdown go; "" writes neither.
	Out     string
	Stall   time.Duration // end the run after this long without progress
	Timeout time.Duration // end the run regardless
	// Keep leaves the world's temp dir in place for debugging.
	Keep bool
	// Bins are prebuilt binaries; nil builds them from the source tree
	// the current directory is in.
	Bins     *e2e.Bins
	Progress io.Writer // nil discards progress
}

// Result is a finished run.
type Result struct {
	Log      Log
	Report   Report
	JSON, MD string // the files written, if Out was set
	Dir      string // the world's temp dir, when kept
}

// Check rejects options the runner can't run yet.
func (o Options) Check() error {
	switch o.Adapter {
	case "", "fake":
	case "claude", "grok", "codex":
		return fmt.Errorf("--adapter %s: real-agent runs are not wired yet (docs/bench.md, Not yet); use --adapter fake", o.Adapter)
	default:
		return fmt.Errorf("--adapter %q: want one of %v", o.Adapter, Adapters)
	}
	switch o.Granularity {
	case "", "file":
	case "symbol":
		return errors.New("--granularity symbol: symbol-level claims are not implemented yet (docs/symbol-claims.md, #329)")
	default:
		return fmt.Errorf("--granularity %q: want one of %v", o.Granularity, Granularities)
	}
	return nil
}

// Run runs one scenario end to end and scores it.
func Run(o Options) (res *Result, err error) {
	if err := o.Check(); err != nil {
		return nil, err
	}
	o.Adapter, o.Granularity = or(o.Adapter, "fake"), or(o.Granularity, "file")
	if o.Stall <= 0 {
		o.Stall = DefaultStall
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Progress == nil {
		o.Progress = io.Discard
	}
	sc, err := Load(o.BenchDir, o.Scenario)
	if err != nil {
		return nil, err
	}
	fixture, err := filepath.Abs(FixtureDir(o.BenchDir))
	if err != nil {
		return nil, err
	}
	root, err := isolatedTemp()
	if err != nil {
		return nil, err
	}
	res = &Result{}
	if o.Keep {
		res.Dir = root
	} else {
		defer func() { _ = os.RemoveAll(root) }()
	}
	bins := o.Bins
	if bins == nil {
		fmt.Fprintln(o.Progress, "building saddle, the fake gh and the fake agent…")
		b, err := e2e.Build(filepath.Join(root, "bin"))
		if err != nil {
			return nil, err
		}
		bins = &b
	}

	t := newRunT(root, o.Progress)
	defer t.close()
	defer func() {
		if p := recover(); p != nil {
			if f, ok := p.(fatal); ok {
				err = fmt.Errorf("bench %s: %s", o.Scenario, f.msg)
			} else {
				err = fmt.Errorf("bench %s: %v", o.Scenario, p)
			}
			res = nil
		}
	}()
	r := &runner{o: o, sc: sc, t: t, plan: MakePlan(DirTree(fixture), sc)}
	r.setup(*bins, fixture)
	r.log.Started = time.Now()
	r.drive()
	r.log.Ended = time.Now()
	r.finish()
	res.Log = r.log
	res.Report = Score(r.log)
	if o.Out != "" {
		if res.JSON, res.MD, err = write(o.Out, res); err != nil {
			return res, err
		}
	}
	return res, nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// runner is one run in progress.
type runner struct {
	o      Options
	sc     *Scenario
	t      *runT
	plan   Plan
	w      *e2e.World
	a      *app.App
	d      *autopilot.Driver
	log    Log
	ticket map[int]*Ticket // issue number -> ticket
	task   map[int]string  // issue number -> task
	freed  map[int]bool    // issues whose held agent was released
}

// setup builds the world: the fixture as the repo's first commit, saddle
// initialised with the fixture's tests as the gate, the tickets filed as
// ready issues in the fake GitHub, autopilot on.
func (r *runner) setup(bins e2e.Bins, fixture string) {
	w := e2e.New(r.t, bins, e2e.Options{NoInit: true})
	r.w = w
	if err := CopyFixture(fixture, w.Repo); err != nil {
		r.t.Fatal(err)
	}
	w.Git(w.Repo, "add", "-A")
	w.Git(w.Repo, "commit", "-q", "-m", "bench fixture")
	w.Git(w.Repo, "push", "-q", "origin", "main")
	w.MustSaddle("init", "-q", "--trust")
	w.WriteConfig(e2e.Options{Top: fmt.Sprintf("concurrency = %d\n", r.sc.cap()), TestCmd: "go test ./..."})

	r.ticket, r.task, r.freed = map[int]*Ticket{}, map[int]string{}, map[int]bool{}
	err := w.GH.Update(func(s *fakegh.State) error {
		for i := range r.sc.Tickets {
			tk := &r.sc.Tickets[i]
			n := s.Next
			s.Next++
			s.Issues = append(s.Issues, &fakegh.Issue{Number: n, Title: tk.Title, Body: tk.Body, State: "OPEN",
				Labels: []string{autopilot.DefaultReadyLabel}})
			r.ticket[n] = tk
		}
		return nil
	})
	if err != nil {
		r.t.Fatal(err)
	}

	r.a = w.App()
	r.d = r.a.NewAutopilot(r.ready)
	r.d.Env = &benchEnv{Env: r.d.Env, r: r}
	if _, err := r.d.Enable(autopilot.Options{}); err != nil {
		r.t.Fatal(err)
	}
	r.log = Log{Scenario: r.sc.Name, Adapter: r.o.Adapter, Granularity: r.o.Granularity, Plan: r.plan,
		Expected: Expected{Overlap: r.sc.Overlap, SafeParallelism: r.sc.SafeParallelism}}
}

// ready lists the fake GitHub's open issues with label, oldest first.
func (r *runner) ready(label string) ([]autopilot.Issue, error) {
	s, err := r.w.GH.Load()
	if err != nil {
		return nil, err
	}
	var out []autopilot.Issue
	for _, is := range s.Issues {
		if strings.EqualFold(is.State, "OPEN") && containsFold(is.Labels, label) {
			out = append(out, autopilot.Issue{Number: is.Number, Title: is.Title, Body: is.Body})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func containsFold(ss []string, s string) bool {
	for _, x := range ss {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// drive ticks autopilot until it stops, the run stalls or times out.
func (r *runner) drive() {
	start := time.Now()
	last, lastSig, lastDecision := start, "", ""
	for {
		rep, err := r.d.Tick()
		if err != nil {
			r.log.EndedEarly = "autopilot tick: " + err.Error()
			return
		}
		if rep.Decision != lastDecision {
			fmt.Fprintf(r.o.Progress, "%5.1fs autopilot: %s\n", time.Since(start).Seconds(), rep.Decision)
			lastDecision = rep.Decision
		}
		for n, why := range rep.Skipped {
			if why != "" && r.task[n] == "" {
				r.log.Waits = append(r.log.Waits, Wait{Issue: n, Why: why})
			}
		}
		if rep.Stopped != "" {
			return
		}
		r.releaseHeld()
		if sig := r.signature(); sig != lastSig {
			last, lastSig = time.Now(), sig
		}
		switch {
		case time.Since(last) > r.o.Stall:
			r.log.EndedEarly = fmt.Sprintf("no progress for %s", r.o.Stall)
			return
		case time.Since(start) > r.o.Timeout:
			r.log.EndedEarly = fmt.Sprintf("timed out after %s", r.o.Timeout)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// signature changes whenever a task makes progress: a new event other
// than autopilot's own, a status change, a fake agent's log growing.
func (r *runner) signature() string {
	var b strings.Builder
	evs, _ := r.a.Store.EventsSince(0)
	for i := len(evs) - 1; i >= 0; i-- {
		if !strings.HasPrefix(evs[i].Kind, "autopilot_") {
			fmt.Fprintf(&b, "e%d;", evs[i].ID)
			break
		}
	}
	ts, _ := r.a.Store.Tasks()
	for _, t := range ts {
		fmt.Fprintf(&b, "%s=%s;", t.ID, t.Status)
		if fi, err := os.Stat(fa.LogPath(r.w.Scripts, t.ID)); err == nil {
			fmt.Fprintf(&b, "%d;", fi.Size())
		}
	}
	return b.String()
}

// releaseHeld messages each held agent whose wait_for tickets' agents have
// all finished their scripts.
func (r *runner) releaseHeld() {
	for n, tk := range r.ticket {
		task := r.task[n]
		if task == "" || len(tk.WaitFor) == 0 || r.freed[n] {
			continue
		}
		ready := true
		for _, want := range tk.WaitFor {
			if !r.finished(r.issueOf(want)) {
				ready = false
				break
			}
		}
		if ready {
			r.freed[n] = true
			if res := r.w.Saddle("message", task, release); res.Code != 0 {
				fmt.Fprintf(r.o.Progress, "could not release %s: %s\n", task, res)
			}
		}
	}
}

// issueOf is the issue a ticket number was filed as.
func (r *runner) issueOf(ticket int) int {
	for n, tk := range r.ticket {
		if tk.Number == ticket {
			return n
		}
	}
	return 0
}

// finished reports whether issue n's fake agent has run its whole script.
func (r *runner) finished(n int) bool {
	task := r.task[n]
	if task == "" {
		return false
	}
	b, _ := os.ReadFile(fa.LogPath(r.w.Scripts, task))
	return strings.Contains(string(b), "idle: script finished") || strings.Contains(string(b), "stuck: script stopped")
}

// finish turns autopilot off and records the tasks, events and the
// integration branch's test result.
func (r *runner) finish() {
	_, _ = r.d.Disable()
	ts, err := r.a.Store.Tasks()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, t := range ts {
		if tk := r.ticket[t.Issue]; tk != nil && t.Role == store.RoleWorker {
			r.log.Tasks = append(r.log.Tasks, TaskRecord{Ticket: tk.Number, Task: t.ID, Status: t.Status})
		}
	}
	evs, err := r.a.Store.EventsSince(0)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, e := range evs {
		r.log.Events = append(r.log.Events, Event{ID: e.ID, TS: e.TS, Task: e.Task, Kind: e.Kind, Data: e.Data})
	}
	r.log.IntegrationGreen, r.log.IntegrationOutput = r.integrationTests()
}

// integrationTests runs the fixture's suite on the integration branch.
func (r *runner) integrationTests() (bool, string) {
	dir := filepath.Join(r.w.Root, "integration-check")
	if res := r.w.Exec(r.w.Repo, "git", "worktree", "add", "-q", "--detach", dir, r.a.Cfg.Integration); res.Code != 0 {
		return false, res.String()
	}
	defer r.w.Exec(r.w.Repo, "git", "worktree", "remove", "--force", dir)
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir, cmd.Env = dir, r.w.Env()
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

// benchEnv is autopilot's Env with Spawn scripted: just before saddle
// spawns a ticket, its edits are applied to the task's base and saved as
// the fake agent's script.
type benchEnv struct {
	autopilot.Env
	r *runner
}

// Route keeps the provider preview of the Env it wraps.
func (e *benchEnv) Route(req autopilot.SpawnReq) (string, string, string, error) {
	if rt, ok := e.Env.(autopilot.Router); ok {
		return rt.Route(req)
	}
	return "", "", "", errors.New("no router")
}

func (e *benchEnv) Spawn(req autopilot.SpawnReq) (string, error) {
	r := e.r
	tk := r.ticket[req.Issue]
	if tk == nil {
		return "", fmt.Errorf("issue #%d is not a bench ticket", req.Issue)
	}
	c := Apply(SpawnBase(r.a.Root, r.a.Cfg.Integration, r.a.Cfg.Base), tk.Edits)
	script := fa.Script{Steps: steps(tk, c)}
	// Spawn starts the agent at once: its script must exist first.
	next, err := r.a.Store.NextID()
	if err != nil {
		return "", err
	}
	if err := script.Save(r.w.Scripts, next); err != nil {
		return "", err
	}
	id, err := e.Env.Spawn(req)
	if err != nil {
		return id, err
	}
	if id != next {
		_ = script.Save(r.w.Scripts, id)
	}
	r.task[req.Issue] = id
	for _, p := range c.Stale {
		r.a.Store.Event(id, EventStale, p)
	}
	return id, nil
}

// steps is the fake agent's script for a change: write, commit, maybe
// wait to be released, done.
func steps(tk *Ticket, c Change) []fa.Step {
	var out []fa.Step
	paths := make([]string, 0, len(c.Writes))
	for p := range c.Writes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		out = append(out, fa.Write(p, c.Writes[p]))
	}
	for _, p := range c.Removes {
		out = append(out, fa.Step{Remove: p})
	}
	out = append(out, fa.Commit(tk.Title))
	if len(tk.WaitFor) > 0 {
		out = append(out, fa.Wait(release))
	}
	return append(out, fa.Done(fmt.Sprintf("Bench ticket %s: %s.", tk.Key(), tk.Title)))
}

// isolatedTemp is a temp dir outside any saddle repo: a world inside one
// would be taken for part of it.
func isolatedTemp() (string, error) {
	parents := []string{os.TempDir(), "/tmp"}
	if c, err := os.UserCacheDir(); err == nil {
		parents = append(parents, filepath.Join(c, "saddle"))
	}
	for _, p := range parents {
		if os.MkdirAll(p, 0o755) != nil {
			continue
		}
		d, err := os.MkdirTemp(p, "bench-")
		if err != nil {
			continue
		}
		if d, err = filepath.EvalSymlinks(d); err == nil && e2e.EnclosingSaddleRepo(d) == "" {
			return d, nil
		}
		_ = os.RemoveAll(d)
	}
	return "", fmt.Errorf("no temp dir outside a saddle repo under %v", parents)
}

// write saves the log and report as JSON and the summary as markdown.
func write(dir string, res *Result) (string, string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	stem := filepath.Join(dir, res.Log.Scenario+"-"+res.Log.Started.UTC().Format("20060102T150405Z"))
	b, err := json.MarshalIndent(struct {
		Report Report `json:"report"`
		Log    Log    `json:"log"`
	}{res.Report, res.Log}, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(stem+".json", append(b, '\n'), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(stem+".md", []byte(Markdown(res.Report)), 0o644); err != nil {
		return stem + ".json", "", err
	}
	return stem + ".json", stem + ".md", nil
}
