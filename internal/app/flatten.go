package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// saddle flatten (#937, ADR 0012) moves a repo that ran the old merge train
// onto one task, one branch, one PR. Each open PR of a landed task is
// replayed onto main with only its task's own commits, force-pushed on its
// own branch and retargeted to main; its task becomes pr_open with base
// main, and the merge queue takes it from there.
//
// A commit in a PR's range <PR base>..<head> is attributed, in this order
// (a commit main itself holds is dropped first):
//
//  1. its Saddle-Task trailer names the task;
//  2. its patch-id is on main (since the head's merge-base with main): it is
//     dropped as on main;
//  3. the train recorded it (a train note's integration range holds its
//     patch-id): the earliest such task (by creation) that is merged, or has
//     an open PR here (itself, or the repair that re-landed it), owns it;
//  4. otherwise the open PRs whose ranges hold the patch-id decide: none
//     but this one, it is the task's own; a PR below it in its base chain,
//     the lowest such PR owns it; only a PR beside it, nobody can tell, and
//     the commit is unattributable.
//
// An unattributable commit leaves the PR alone, with the reason. A PR whose
// own commits are all on main is closed with a comment naming them, and its
// task is merged. A replay that conflicts leaves the PR as it was and
// reopens the task as fixing (#936), to be fixed on the same branch.
//
// PRs are handled bottom-up (the base chain's lowest first). --dry-run
// changes no ref, PR or task: it replays in a scratch worktree to say what
// would conflict, and prints the plan.

// Event kinds of saddle flatten.
const (
	EventFlattened       = "flattened"        // data: url, old base → main, head
	EventFlattenClosed   = "flatten_closed"   // its commits were all on main; data: url and the commits
	EventFlattenLeft     = "flatten_left"     // left alone; data: why
	EventFlattenConflict = "flatten_conflict" // the replay conflicted; data: files
	EventFlattenFolded   = "flatten_folded"   // an original folded into its repair's PR; data: the repair
	EventFlattenMoved    = "flatten_moved"    // a conflicting task's local branch was moved onto main; data: old → new
)

// KilledBySupersededByFlatten is an original task whose work a repair
// re-landed: flatten keeps the repair's PR and kills the original.
const KilledBySupersededByFlatten = "superseded_by_flatten"

// Flatten actions.
const (
	FlattenReplay = "flatten" // replay its own commits onto main, push, retarget
	FlattenClose  = "close"   // its own commits are all on main
	FlattenFix    = "fixing"  // the replay conflicts: reopen the task
	FlattenLeave  = "leave"   // left alone; see Reason
)

// FlattenForge is the part of the forge flatten uses; forge.Forge is one.
// A forge that can also retitle a PR (SetPRTitle) gets a repair's title
// without its "Repair tN " prefix.
type FlattenForge interface {
	OpenPRs(ctx context.Context) ([]forge.PR, error)
	SetPRBase(ctx context.Context, ref, base string) error
	SetPRBody(ctx context.Context, ref, body string) error
	ClosePR(ctx context.Context, ref string) error
	AddComment(ctx context.Context, ref, body string) error
}

type prTitler interface {
	SetPRTitle(ctx context.Context, ref, title string) error
}

// FlattenCommit is one commit of a PR's range and where it goes.
type FlattenCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	// Owner is the task that owns a dropped commit, "main" for one already
	// on main, or "" for the PR's own and unattributable ones.
	Owner string `json:"owner,omitempty"`
	Why   string `json:"why,omitempty"`
}

// FlattenPR is one open PR's plan and, after a real run, its outcome.
type FlattenPR struct {
	Task    string `json:"task"`
	Title   string `json:"title"`
	PR      string `json:"pr"`
	Number  int    `json:"number"`
	Branch  string `json:"branch"`
	OldBase string `json:"old_base"`
	NewBase string `json:"new_base"`
	// Supersedes is the original task this repair re-landed; it is killed
	// with killed_by superseded_by_flatten and the PR drops "Repair tN ".
	Supersedes     string          `json:"supersedes,omitempty"`
	NewTitle       string          `json:"new_title,omitempty"`
	Keep           []FlattenCommit `json:"keep"`
	Drop           []FlattenCommit `json:"drop"`
	Unattributable []FlattenCommit `json:"unattributable,omitempty"`
	Action         string          `json:"action"`
	Reason         string          `json:"reason,omitempty"`
	Conflicts      []string        `json:"conflicts,omitempty"`
	// Outcome is what a real run did: the action, or "failed: <why>".
	Outcome string `json:"outcome,omitempty"`
	head    string
	newHead string
	// On a conflict: main plus the own commits that applied before it, and
	// the own commits from the conflicting one on.
	cleanHead string
	rest      []FlattenCommit
}

// FlattenResult is a flatten run.
type FlattenResult struct {
	DryRun bool        `json:"dry_run"`
	Base   string      `json:"base"`
	PRs    []FlattenPR `json:"prs"`
	// LegacyTrainOff says the run set legacy_train = false.
	LegacyTrainOff bool `json:"legacy_train_off,omitempty"`
}

var repairTitleRe = regexp.MustCompile(`^Repair (t\d+) `)

// flatCommit is a commit of a PR's range with its trailer and patch-id.
type flatCommit struct {
	sha, subject, pid string
	trailers          []string
	onMain            bool // the commit itself is on main
}

// flatPR is a candidate PR while flatten plans.
type flatPR struct {
	t       store.Task
	p       forge.PR
	baseRef string
	headRef string
	depth   int
	below   []string // tasks of the open PRs under it in its base chain, nearest first
	commits []flatCommit
	pids    map[string]bool
	plan    FlattenPR
}

// Flatten plans, and unless dryRun applies, saddle flatten over f (nil is
// the repo's forge).
func (a *App) Flatten(ctx context.Context, f FlattenForge, dryRun bool) (FlattenResult, error) {
	res := FlattenResult{DryRun: dryRun, Base: a.Cfg.Base}
	if f == nil {
		f = a.Forge()
	}
	r := a.remote()
	if r == "" {
		return res, errors.New("this repo has no remote, so it has no PRs to flatten")
	}
	open, err := f.OpenPRs(ctx)
	if err != nil {
		return res, fmt.Errorf("list open PRs: %w", err)
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return res, err
	}
	byBranch := map[string]store.Task{}
	byID := map[string]store.Task{}
	for _, t := range ts {
		byID[t.ID] = t
		if t.Branch != "" {
			byBranch[t.Branch] = t
		}
	}
	var prs []*flatPR
	for _, p := range open {
		t, ok := byBranch[p.Head]
		if !ok || t.Role != store.RoleWorker || t.Status != store.Landed {
			continue // not a PR of the old train's
		}
		prs = append(prs, &flatPR{t: t, p: p})
	}
	if len(prs) == 0 {
		return res, a.flattenLegacyOff(&res)
	}
	mainRef := "refs/remotes/" + r + "/" + a.Cfg.Base
	specs := []string{"+refs/heads/" + a.Cfg.Base + ":" + mainRef}
	for _, fp := range prs {
		fp.headRef = "refs/remotes/" + r + "/" + fp.p.Head
		specs = append(specs, "+refs/heads/"+fp.p.Head+":"+fp.headRef)
	}
	if _, err := gitx.Run(a.Root, append([]string{"fetch", "--quiet", r}, specs...)...); err != nil {
		return res, fmt.Errorf("fetch: %w", err)
	}
	mainTip, err := gitx.RevParse(a.Root, mainRef)
	if err != nil {
		return res, err
	}
	byHead := map[string]*flatPR{}
	byTask := map[string]*flatPR{}
	for _, fp := range prs {
		byHead[fp.p.Head] = fp
		byTask[fp.t.ID] = fp
		fp.baseRef = mainRef
		if fp.p.Base != "" && fp.p.Base != a.Cfg.Base {
			ref := "refs/remotes/" + r + "/" + fp.p.Base
			if _, err := gitx.Run(a.Root, "fetch", "--quiet", r, "+refs/heads/"+fp.p.Base+":"+ref); err == nil {
				fp.baseRef = ref
			}
		}
	}
	// The base chain: each PR's open PRs below it, nearest first.
	for _, fp := range prs {
		seen := map[string]bool{fp.t.ID: true}
		for b := fp.p.Base; ; {
			q, ok := byHead[b]
			if !ok || seen[q.t.ID] {
				break
			}
			seen[q.t.ID] = true
			fp.below = append(fp.below, q.t.ID)
			b = q.p.Base
		}
		fp.depth = len(fp.below)
	}
	sort.SliceStable(prs, func(i, j int) bool {
		if prs[i].depth != prs[j].depth {
			return prs[i].depth < prs[j].depth
		}
		return prs[i].p.Number < prs[j].p.Number
	})
	for _, fp := range prs {
		if fp.commits, err = rangeCommits(a.Root, fp.baseRef+".."+fp.headRef); err != nil {
			return res, err
		}
		// A base that lacks main brings main's own commits into the range.
		off, err := gitx.Run(a.Root, "rev-list", fp.baseRef+".."+fp.headRef, "^"+mainRef)
		if err != nil {
			return res, err
		}
		notOnMain := map[string]bool{}
		for _, s := range strings.Fields(off) {
			notOnMain[s] = true
		}
		fp.pids = map[string]bool{}
		for i, c := range fp.commits {
			fp.commits[i].onMain = !notOnMain[c.sha]
			fp.pids[c.pid] = true
		}
	}
	// Repairs: an original whose work a repair with an open PR here re-landed.
	repairer := map[string]string{} // original → repair
	for _, fp := range prs {
		orig, ok := a.repairedBy(fp.t.ID)
		if !ok {
			if m := repairTitleRe.FindStringSubmatch(fp.t.Title); m != nil {
				orig, ok = m[1], true
			}
		}
		if ok && orig != fp.t.ID {
			if _, has := byID[orig]; has {
				repairer[orig] = fp.t.ID
				fp.plan.Supersedes = orig
				fp.plan.NewTitle = strings.TrimSpace(repairTitleRe.ReplaceAllString(fp.t.Title, ""))
			}
		}
	}
	trainMerged := map[string]bool{}
	claims, err := a.trainClaims(trainMerged)
	if err != nil {
		return res, err
	}
	isMerged := func(id string) bool { return trainMerged[id] || byID[id].Status == store.Merged }
	// carrier is where task x's work goes: its own open PR here, the PR of
	// the repair that re-landed it, "main" when it merged, or "".
	carrier := func(x string) string {
		if _, ok := byTask[x]; ok {
			return x
		}
		if rp, ok := repairer[x]; ok {
			return rp
		}
		if isMerged(x) {
			return "main"
		}
		return ""
	}
	mainPIDs := map[string]map[string]string{} // merge-base → pid → sha on main
	for _, fp := range prs {
		mb, err := gitx.Run(a.Root, "merge-base", mainRef, fp.headRef)
		if err != nil {
			mb = ""
		}
		onMain, ok := mainPIDs[mb]
		if !ok {
			rng := mainRef
			if mb != "" {
				rng = mb + ".." + mainRef
			}
			cs, err := rangeCommits(a.Root, rng)
			if err != nil {
				return res, err
			}
			onMain = map[string]string{}
			for _, c := range cs {
				onMain[c.pid] = c.sha
			}
			mainPIDs[mb] = onMain
		}
		a.attribute(fp, onMain, claims, byID, byTask, carrier, prs)
	}
	for _, fp := range prs {
		a.planFlatten(fp, mainRef, mainTip)
		if !dryRun {
			a.applyFlatten(ctx, f, r, fp, mainTip)
		}
		res.PRs = append(res.PRs, fp.plan)
	}
	if dryRun {
		return res, nil
	}
	return res, a.flattenLegacyOff(&res)
}

// flattenLegacyOff turns legacy_train off after a real run, if it was on.
func (a *App) flattenLegacyOff(res *FlattenResult) error {
	if res.DryRun || !a.Cfg.LegacyTrain {
		return nil
	}
	if _, err := a.SetConfig("legacy_train", "false"); err != nil {
		return fmt.Errorf("set legacy_train = false: %w", err)
	}
	a.Cfg.LegacyTrain = false
	res.LegacyTrainOff = true
	return nil
}

// trainClaims maps each patch-id in a train note's integration range to the
// tasks whose range holds it, earliest task first. merged gets the tasks
// the train recorded as merged.
func (a *App) trainClaims(merged map[string]bool) (map[string][]string, error) {
	es, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	created := map[string]int64{}
	if ts, err := a.Store.Tasks(); err == nil {
		for _, t := range ts {
			created[t.ID] = t.CreatedAt.UnixNano()
		}
	}
	out := map[string][]string{}
	for _, e := range es {
		if e.State == TrainMerged {
			merged[e.Task] = true
		}
		from, to, ok := strings.Cut(e.Note, "..")
		if !ok || from == "" || to == "" || strings.ContainsAny(e.Note, " \n") {
			continue
		}
		cs, err := rangeCommits(a.Root, from+".."+to)
		if err != nil {
			continue // the range is gone; nothing to learn from it
		}
		for _, c := range cs {
			if !slices.Contains(out[c.pid], e.Task) {
				out[c.pid] = append(out[c.pid], e.Task)
			}
		}
	}
	for pid := range out {
		slices.SortStableFunc(out[pid], func(x, y string) int {
			switch {
			case created[x] < created[y]:
				return -1
			case created[x] > created[y]:
				return 1
			}
			return compareTaskIDs(x, y)
		})
	}
	return out, nil
}

// attribute sorts fp's commits into its own, dropped and unattributable.
func (a *App) attribute(fp *flatPR, onMain map[string]string, claims map[string][]string, byID map[string]store.Task,
	byTask map[string]*flatPR, carrier func(string) string, all []*flatPR) {
	own := func(x string) bool { return x == fp.t.ID || x == fp.plan.Supersedes }
	label := func(x string) string {
		if q, ok := byTask[x]; ok {
			return fmt.Sprintf("%s's (#%d)", x, q.p.Number)
		}
		return x + "'s"
	}
	for _, c := range fp.commits {
		fc := FlattenCommit{SHA: c.sha, Subject: c.subject}
		keep := func() { fp.plan.Keep = append(fp.plan.Keep, fc) }
		drop := func(owner, why string) {
			fc.Owner, fc.Why = owner, why
			fp.plan.Drop = append(fp.plan.Drop, fc)
		}
		unknown := func(why string) {
			fc.Why = why
			fp.plan.Unattributable = append(fp.plan.Unattributable, fc)
		}
		if c.onMain {
			drop("main", "on main")
			continue
		}
		// 1. The trailer.
		if len(c.trailers) > 0 {
			if slices.ContainsFunc(c.trailers, own) {
				keep()
				continue
			}
			x := c.trailers[0]
			switch to := carrier(x); {
			case to == "main":
				drop("main", fmt.Sprintf("Saddle-Task: %s, merged into main", x))
			case to != "" && own(to):
				keep()
			case to != "":
				drop(to, "Saddle-Task: "+x+", "+label(to))
			default:
				unknown(fmt.Sprintf("Saddle-Task: %s, which has no open PR and isn't merged", strings.Join(c.trailers, ", ")))
			}
			continue
		}
		// 2. Already on main.
		if sha, ok := onMain[c.pid]; ok {
			drop("main", "on main as "+short(sha))
			continue
		}
		// 3. The train's record.
		decided := false
		for _, x := range claims[c.pid] {
			if carrier(x) == "main" {
				drop("main", x+"'s, merged into main")
				decided = true
				break
			}
		}
		if !decided {
			for _, x := range claims[c.pid] {
				to := carrier(x)
				if to == "" {
					continue
				}
				if own(to) {
					keep()
				} else {
					why := label(to)
					if to != x {
						why = x + "'s, re-landed by " + label(to)
					}
					drop(to, why)
				}
				decided = true
				break
			}
		}
		if decided {
			continue
		}
		// 4. The open PRs' ranges.
		owner := ""
		for i := len(fp.below) - 1; i >= 0; i-- { // lowest first
			if q := byTask[fp.below[i]]; q.pids[c.pid] {
				owner = q.t.ID
				break
			}
		}
		if owner != "" {
			drop(owner, label(owner)+", the PR below it")
			continue
		}
		var beside []string
		for _, q := range all {
			if q == fp || !q.pids[c.pid] || slices.Contains(q.below, fp.t.ID) {
				continue // itself, or a PR stacked above it
			}
			beside = append(beside, fmt.Sprintf("#%d (%s)", q.p.Number, q.t.ID))
		}
		if len(beside) > 0 {
			unknown("no trailer, and also in " + strings.Join(beside, ", "))
			continue
		}
		keep()
	}
}

// planFlatten decides fp's action, replaying in a scratch worktree to find
// conflicts. Nothing it does moves a ref.
func (a *App) planFlatten(fp *flatPR, mainRef, mainTip string) {
	pl := &fp.plan
	t := fp.t
	pl.Task, pl.Title, pl.PR, pl.Number, pl.Branch = t.ID, t.Title, fp.p.URL, fp.p.Number, fp.p.Head
	pl.OldBase, pl.NewBase, pl.head = fp.p.Base, a.Cfg.Base, fp.p.HeadSHA
	if pl.head == "" {
		pl.head, _ = gitx.RevParse(a.Root, fp.headRef)
	}
	if pl.Keep == nil {
		pl.Keep = []FlattenCommit{}
	}
	if pl.Drop == nil {
		pl.Drop = []FlattenCommit{}
	}
	switch {
	case len(pl.Unattributable) > 0:
		pl.Action = FlattenLeave
		pl.Reason = fmt.Sprintf("%s can't be attributed to one task; flatten leaves this PR alone. Give each commit a Saddle-Task trailer, or move it to its own task's branch, and run flatten again",
			plural(len(pl.Unattributable), "commit"))
		return
	case len(pl.Keep) == 0:
		var notMain []string
		for _, d := range pl.Drop {
			if d.Owner != "main" && !slices.Contains(notMain, d.Owner) {
				notMain = append(notMain, d.Owner)
			}
		}
		if len(notMain) > 0 || len(pl.Drop) == 0 {
			pl.Action = FlattenLeave
			pl.Reason = "it has no commits of its own"
			if len(notMain) > 0 {
				pl.Reason += "; its commits are " + strings.Join(notMain, ", ") + "'s"
			}
			return
		}
		pl.Action = FlattenClose
		pl.Reason = "its commits are all on " + a.Cfg.Base
		return
	}
	// Already flat: its range is main..head and every commit is its own.
	if fp.baseRef == mainRef && len(pl.Drop) == 0 {
		if _, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", mainRef, fp.headRef); err == nil {
			pl.Action, pl.newHead = FlattenReplay, pl.head
			pl.Reason = "already on " + a.Cfg.Base + " with only its own commits"
			return
		}
	}
	head, conflicts, at, err := a.replayOwn(mainTip, pl.Keep)
	switch {
	case err != nil:
		pl.Action, pl.Reason = FlattenLeave, "replaying its commits failed: "+oneLine(err.Error())
	case len(conflicts) > 0:
		pl.Action, pl.Conflicts = FlattenFix, conflicts
		pl.cleanHead, pl.rest = head, pl.Keep[at:]
		pl.Reason = "replaying its own commits onto " + a.Cfg.Base + " conflicts in " + strings.Join(conflicts, ", ")
	case head == mainTip:
		pl.Action, pl.Reason = FlattenClose, "its commits' changes are all on "+a.Cfg.Base+" already"
	default:
		pl.Action, pl.newHead = FlattenReplay, head
	}
}

// flattenScratch is flatten's own worktree for replays.
func (a *App) flattenScratch() string { return a.stateDir("flatten") }

func (a *App) cleanFlattenScratch() {
	dir := a.flattenScratch()
	if _, err := os.Stat(dir); err == nil {
		_, _ = trainGit(dir, "cherry-pick", "--abort")
		_ = gitx.WorktreeRemove(a.Root, dir)
		_ = os.RemoveAll(dir)
	}
	_, _ = gitx.Run(a.Root, "worktree", "prune")
}

// replayOwn cherry-picks cs, oldest first, onto onto in the scratch
// worktree. It returns the new head; or, on a conflict, the head before
// the conflicting commit, the files it conflicts in and its index in cs. A
// commit whose change onto already has is dropped.
func (a *App) replayOwn(onto string, cs []FlattenCommit) (string, []string, int, error) {
	a.cleanFlattenScratch()
	defer a.cleanFlattenScratch()
	dir := a.flattenScratch()
	if err := os.MkdirAll(a.stateDir(), 0o755); err != nil {
		return "", nil, 0, err
	}
	if _, err := gitx.Run(a.Root, "worktree", "add", "--detach", dir, onto); err != nil {
		return "", nil, 0, err
	}
	for i, c := range cs {
		if _, err := trainGit(dir, "-c", "rerere.enabled=false", "cherry-pick", "--empty=drop", c.SHA); err != nil {
			conf, _ := gitx.Run(dir, "diff", "--name-only", "--diff-filter=U")
			if conf != "" {
				_, _ = trainGit(dir, "cherry-pick", "--abort")
				head, herr := gitx.RevParse(dir, "HEAD")
				return head, strings.Split(conf, "\n"), i, herr
			}
			if _, serr := trainGit(dir, "cherry-pick", "--skip"); serr != nil {
				return "", nil, 0, fmt.Errorf("cherry-pick %s: %w", short(c.SHA), err)
			}
		}
	}
	head, err := gitx.RevParse(dir, "HEAD")
	return head, nil, 0, err
}

// applyFlatten carries out fp's plan. A failure is fp's outcome; the other
// PRs go on.
func (a *App) applyFlatten(ctx context.Context, f FlattenForge, r string, fp *flatPR, mainTip string) {
	pl := &fp.plan
	t := fp.t
	var err error
	switch pl.Action {
	case FlattenLeave:
		a.Store.Event(t.ID, EventFlattenLeft, pl.PR+": "+pl.Reason)
	case FlattenReplay:
		err = a.flattenReplay(ctx, f, r, fp)
	case FlattenClose:
		err = a.flattenClose(ctx, f, fp, mainTip)
	case FlattenFix:
		err = a.flattenFix(fp)
	}
	pl.Outcome = pl.Action
	if err != nil {
		pl.Outcome = "failed: " + oneLine(err.Error())
		a.Store.Event(t.ID, EventFlattenLeft, pl.PR+": "+pl.Outcome)
	}
}

func (a *App) flattenReplay(ctx context.Context, f FlattenForge, r string, fp *flatPR) error {
	pl := &fp.plan
	t := fp.t
	if pl.newHead != pl.head {
		if _, err := trainGit(a.Root, "push", "--force-with-lease=refs/heads/"+pl.Branch+":"+pl.head, r, pl.newHead+":refs/heads/"+pl.Branch); err != nil {
			return fmt.Errorf("push %s: %w", pl.Branch, err)
		}
	}
	if pl.OldBase != a.Cfg.Base {
		if err := f.SetPRBase(ctx, pl.PR, a.Cfg.Base); err != nil {
			return fmt.Errorf("retarget %s: %w", pl.PR, err)
		}
	}
	if pl.NewTitle != "" {
		if tf, ok := f.(prTitler); ok {
			if err := tf.SetPRTitle(ctx, pl.PR, pl.NewTitle); err != nil {
				return fmt.Errorf("retitle %s: %w", pl.PR, err)
			}
		}
		if err := a.Store.SetField(t.ID, "title", pl.NewTitle); err != nil {
			return err
		}
		t.Title = pl.NewTitle
	}
	if err := f.SetPRBody(ctx, pl.PR, a.donePRBody(t)); err != nil {
		return fmt.Errorf("rewrite the body of %s: %w", pl.PR, err)
	}
	// The local branch follows when no worktree has it checked out.
	if gitx.BranchExists(a.Root, pl.Branch) {
		if _, err := trainGit(a.Root, "branch", "--force", pl.Branch, pl.newHead); err != nil {
			a.Store.Event(t.ID, EventFlattenLeft, "local branch "+pl.Branch+" kept: "+oneLine(err.Error()))
		}
	}
	if err := a.flattenToNew(t, pl.PR, store.PROpen); err != nil {
		return err
	}
	a.Store.Event(t.ID, EventFlattened, fmt.Sprintf("%s %s → %s head=%s kept=%d dropped=%d", pl.PR, pl.OldBase, a.Cfg.Base, short(pl.newHead), len(pl.Keep), len(pl.Drop)))
	return a.foldOriginal(fp)
}

func (a *App) flattenClose(ctx context.Context, f FlattenForge, fp *flatPR, mainTip string) error {
	pl := &fp.plan
	var lines []string
	for _, d := range pl.Drop {
		lines = append(lines, fmt.Sprintf("- %s %s (%s)", short(d.SHA), d.Subject, d.Why))
	}
	for _, k := range pl.Keep {
		lines = append(lines, fmt.Sprintf("- %s %s (its change is already in %s at %s)", short(k.SHA), k.Subject, a.Cfg.Base, short(mainTip)))
	}
	body := fmt.Sprintf("saddle flatten: this PR's work is already on %s, so it is closed.\n\n%s", a.Cfg.Base, strings.Join(lines, "\n"))
	if err := f.AddComment(ctx, pl.PR, body); err != nil {
		return fmt.Errorf("comment on %s: %w", pl.PR, err)
	}
	if err := f.ClosePR(ctx, pl.PR); err != nil {
		return fmt.Errorf("close %s: %w", pl.PR, err)
	}
	if err := a.flattenToNew(fp.t, pl.PR, store.Merged); err != nil {
		return err
	}
	a.Store.Event(fp.t.ID, EventFlattenClosed, pl.PR+" "+pl.Reason)
	return a.foldOriginal(fp)
}

// flattenFix leaves the PR as it was and reopens its task to fix the
// conflict on its own branch, against main.
func (a *App) flattenFix(fp *flatPR) error {
	pl := &fp.plan
	t := fp.t
	if err := a.flattenToNew(t, pl.PR, store.PROpen); err != nil {
		return err
	}
	reason := "conflict: " + strings.Join(pl.Conflicts, ", ")
	if why := a.moveToOwnWork(t, pl); why != "" {
		reason += "; its branch was not moved: " + why
	} else {
		var picks []string
		for _, c := range pl.rest {
			picks = append(picks, short(c.SHA)+" "+c.Subject)
		}
		reason += flattenPicks + strings.Join(picks, ", ")
	}
	if err := a.Store.Reopen(t.ID, reason, 1); err != nil {
		return err
	}
	var own []string
	for _, k := range pl.Keep {
		own = append(own, short(k.SHA)+" "+k.Subject)
	}
	a.Store.Event(t.ID, EventFlattenConflict, strings.Join(pl.Conflicts, ", ")+"; its own commits: "+strings.Join(own, "; "))
	a.Store.Event(t.ID, EventFixOpened, fmt.Sprintf("attempt 1/%d: %s (saddle flatten)", a.fixAttempts(), reason))
	return a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"saddle flatten: %s %q's own commits conflict with %s in %s, so its PR %s was left as it was and %s is reopened to fix it on its own branch.",
		t.ID, t.Title, a.Cfg.Base, strings.Join(pl.Conflicts, ", "), pl.PR, t.ID))
}

// flattenPicks marks a fix reason whose task flatten moved onto main: the
// commits after it are the ones left to cherry-pick, in order.
const flattenPicks = "; cherry-pick in order: "

// moveToOwnWork points t's local branch at main plus its own commits that
// replayed cleanly, so its fix starts from its own work on main and not
// the old stacked branch. Its PR keeps the old head until done pushes. It
// says why when it can't: a live agent, a worktree not on the branch or
// with uncommitted changes.
func (a *App) moveToOwnWork(t store.Task, pl *FlattenPR) string {
	if pl.cleanHead == "" {
		return "the replay left no head"
	}
	if a.agentAlive(t, time.Now()) {
		return "its agent is alive"
	}
	if fi, err := os.Stat(t.Worktree); t.Worktree == "" || err != nil || !fi.IsDir() {
		// No worktree: move the branch itself; relaunch recreates one on it.
		if !gitx.BranchExists(a.Root, t.Branch) {
			return "it has no local branch"
		}
		if _, err := trainGit(a.Root, "branch", "--force", t.Branch, pl.cleanHead); err != nil {
			return "moving " + t.Branch + " failed: " + oneLine(err.Error())
		}
		a.Store.Event(t.ID, EventFlattenMoved, t.Branch+" → "+short(pl.cleanHead))
		return ""
	}
	if cur, err := gitx.Run(t.Worktree, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil || cur != t.Branch {
		return "its worktree isn't on " + t.Branch
	}
	if gitx.RebaseInProgress(t.Worktree) {
		return "its worktree has a rebase in progress"
	}
	if _, err := gitx.Run(t.Worktree, "rev-parse", "-q", "--verify", "CHERRY_PICK_HEAD"); err == nil {
		return "its worktree has a cherry-pick in progress"
	}
	if dirty, err := gitx.Dirty(t.Worktree); err != nil || len(dirty) > 0 {
		return "its worktree has uncommitted changes"
	}
	old, _ := gitx.RevParse(t.Worktree, "HEAD")
	if _, err := trainGit(t.Worktree, "reset", "--quiet", "--hard", pl.cleanHead); err != nil {
		return "reset failed: " + oneLine(err.Error())
	}
	a.Store.Event(t.ID, EventFlattenMoved, t.Branch+" "+short(old)+" → "+short(pl.cleanHead))
	return ""
}

// flattenToNew moves t from the old train to the new model: base main, its
// PR recorded, status set, and its train entry and old stack records gone.
func (a *App) flattenToNew(t store.Task, pr, status string) error {
	if err := a.Store.SetField(t.ID, "base", a.Cfg.Base); err != nil {
		return err
	}
	if t.PR != pr {
		if err := a.Store.SetField(t.ID, "pr", pr); err != nil {
			return err
		}
	}
	if err := a.Store.Revive(t.ID, status); err != nil {
		return err
	}
	return a.forgetLegacy(t.ID)
}

// foldOriginal kills the original a repair re-landed: the repair's PR now
// carries its work.
func (a *App) foldOriginal(fp *flatPR) error {
	o := fp.plan.Supersedes
	if o == "" {
		return nil
	}
	if ot, err := a.Store.Task(o); err == nil && ot.Status == store.Merged {
		return nil
	}
	if err := a.Store.KillBy(o, KilledBySupersededByFlatten); err != nil {
		return err
	}
	a.Store.Event(o, EventFlattenFolded, fp.t.ID+" "+fp.plan.PR)
	return a.forgetLegacy(o)
}

// forgetLegacy drops task's old train records: its train entry, its
// independently published record, its saved PR base and its custom stacks.
func (a *App) forgetLegacy(task string) error {
	errs := []error{a.Store.Dequeue(task), a.forgetPublished(task)}
	if m := a.savedBases(); m[task] != "" {
		delete(m, task)
		errs = append(errs, a.writeBases(m))
	}
	if sf, err := a.loadStacks(); err == nil && len(sf.Stacks) > 0 {
		changed := false
		for i := range sf.Stacks {
			n := len(sf.Stacks[i].Tasks)
			sf.Stacks[i].Tasks = slices.DeleteFunc(sf.Stacks[i].Tasks, func(id string) bool { return id == task })
			changed = changed || n != len(sf.Stacks[i].Tasks)
		}
		if changed {
			sf.Stacks = slices.DeleteFunc(sf.Stacks, func(s CustomStack) bool { return len(s.Tasks) == 0 })
			errs = append(errs, a.saveStacks(sf))
		}
	}
	return errors.Join(errs...)
}

// rangeCommits lists the non-merge commits of rng, oldest first, with
// their subjects, Saddle-Task trailers and stable patch-ids.
func rangeCommits(dir, rng string) ([]flatCommit, error) {
	out, err := gitx.Run(dir, "log", "--reverse", "--no-merges", "--format=%H%x1f%s%x1f%(trailers:key=Saddle-Task,valueonly,separator=%x2C)%x1e", rng)
	if err != nil {
		return nil, fmt.Errorf("git log %s: %w", rng, err)
	}
	var cs []flatCommit
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 3)
		if len(f) < 3 {
			continue
		}
		c := flatCommit{sha: f[0], subject: f[1]}
		for _, id := range strings.Split(f[2], ",") {
			if id = strings.TrimSpace(id); id != "" {
				c.trailers = append(c.trailers, id)
			}
		}
		cs = append(cs, c)
	}
	if len(cs) == 0 {
		return nil, nil
	}
	log, err := gitx.Run(dir, "log", "-p", "--no-merges", "--no-color", "--no-ext-diff", "--format=commit %H", rng)
	if err != nil {
		return nil, err
	}
	ids, err := gitx.PatchID(dir, log)
	if err != nil {
		return nil, err
	}
	pid := map[string]string{}
	for _, l := range strings.Split(ids, "\n") {
		if id, sha, ok := strings.Cut(strings.TrimSpace(l), " "); ok {
			pid[sha] = id
		}
	}
	for i := range cs {
		if p, ok := pid[cs[i].sha]; ok {
			cs[i].pid = p
		} else {
			cs[i].pid = "empty:" + cs[i].sha // no diff: matches nothing else
		}
	}
	return cs, nil
}

// Text is the run as the CLI prints it.
func (r FlattenResult) Text() string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("saddle flatten --dry-run: nothing was changed.\n")
	}
	if len(r.PRs) == 0 {
		b.WriteString("No open PRs of the old merge train; nothing to flatten.\n")
	}
	for _, p := range r.PRs {
		fmt.Fprintf(&b, "\n#%d %s %q (%s)\n", p.Number, p.Task, p.Title, p.Branch)
		fmt.Fprintf(&b, "  base: %s → %s\n", p.OldBase, p.NewBase)
		if p.Supersedes != "" {
			fmt.Fprintf(&b, "  repair of %s: title → %q; %s is killed (killed_by %s)\n", p.Supersedes, p.NewTitle, p.Supersedes, KilledBySupersededByFlatten)
		}
		for _, c := range p.Keep {
			fmt.Fprintf(&b, "  keep  %s %s\n", short7(c.SHA), c.Subject)
		}
		for _, c := range p.Drop {
			fmt.Fprintf(&b, "  drop  %s %s  [%s]\n", short7(c.SHA), c.Subject, c.Why)
		}
		for _, c := range p.Unattributable {
			fmt.Fprintf(&b, "  ????  %s %s  [unattributable: %s]\n", short7(c.SHA), c.Subject, c.Why)
		}
		verb := map[string]string{
			FlattenReplay: "replay its own commits onto " + p.NewBase + ", force-push " + p.Branch + ", retarget the PR to " + p.NewBase + ", body ← task summary; task → pr_open",
			FlattenClose:  "close the PR with a comment naming the commits on " + p.NewBase + "; task → merged",
			FlattenFix:    "leave the PR as it is; reopen " + p.Task + " as fixing (conflict: " + strings.Join(p.Conflicts, ", ") + ")",
			FlattenLeave:  "leave the PR alone",
		}[p.Action]
		if p.Action == FlattenReplay && p.Reason != "" {
			verb = "retarget the PR to " + p.NewBase + " (" + p.Reason + "), body ← task summary; task → pr_open"
		}
		if r.DryRun {
			fmt.Fprintf(&b, "  plan: %s\n", verb)
		} else {
			fmt.Fprintf(&b, "  did:  %s\n", p.Outcome)
		}
		if p.Reason != "" && p.Action != FlattenReplay {
			fmt.Fprintf(&b, "  why:  %s\n", p.Reason)
		}
	}
	if r.LegacyTrainOff {
		b.WriteString("\nlegacy_train was on; it is now set to false in .saddle/config.toml. The merge queue merges these PRs from now on.\n")
	}
	return b.String()
}

func short7(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// writeBases replaces .saddle/pr-bases.json with m.
func (a *App) writeBases(m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.prBasesPath() + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.prBasesPath())
}

// compareTaskIDs orders t9 before t10.
func compareTaskIDs(x, y string) int {
	nx, ex := strconv.Atoi(strings.TrimPrefix(x, "t"))
	ny, ey := strconv.Atoi(strings.TrimPrefix(y, "t"))
	if ex == nil && ey == nil && nx != ny {
		if nx < ny {
			return -1
		}
		return 1
	}
	return strings.Compare(x, y)
}
