package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/secretscan"
	"github.com/brandonapol/saddle/internal/store"
)

// History-scanning checks (#405). GitGuardian, gitleaks-action, TruffleHog,
// DCO bots and commit-message linters read every commit of a PR, not its
// tree, so a commit that fixes the tree leaves them red. Saddle handles them
// in three places:
//   - the land gate scans each commit's added lines for secret-shaped
//     literals (secretscan's patterns, plus gitleaks when it is installed)
//     and bounces the branch to its agent before it ever lands;
//   - a red history check on a layer saddle owns and hasn't merged rewrites
//     that layer (its commits squashed into one, same tree) and force-pushes
//     with lease, instead of spawning a "Fix CI" task that adds commits; one
//     it can't rewrite raises a single decision card and stops retrying;
//   - a CI repair whose own PR goes red is never repaired again: the spawn is
//     refused by title, and the red layer is escalated by lineage.
// See docs/history-checks.md.

// knownHistoryChecks matches the check names of known history scanners.
var knownHistoryChecks = regexp.MustCompile(`(?i)\b(?:git ?guardian|ggshield|gitleaks|truffle ?hog|dco|sign[- ]?off|commitlint|commit[- ]?messages?)\b`)

// isHistoryCheck reports whether check (a "workflow / job" label) is a
// history scanner: a known one, or one of [ci] history_checks.
func isHistoryCheck(check string, extra []string) bool {
	if knownHistoryChecks.MatchString(check) {
		return true
	}
	lc := strings.ToLower(check)
	return slices.ContainsFunc(extra, func(e string) bool {
		return e != "" && strings.Contains(lc, strings.ToLower(e))
	})
}

// scannerIgnoreFiles are the files scanners read to skip findings. Adding
// one proves nothing until the check passes (#405).
var scannerIgnoreFiles = []string{".gitguardian.yaml", ".gitguardian.yml", ".gitleaksignore", ".gitleaks.toml", ".trufflehogignore"}

// allowPragma marks a line as a known false positive, as gitleaks does.
const allowPragma = "gitleaks:allow"

// secretHit is a secret-shaped literal a commit adds.
type secretHit struct {
	Commit string
	File   string
	Line   int
	Rule   string // the gitleaks rule, "" for secretscan's patterns
}

func (h secretHit) String() string {
	return fmt.Sprintf("commit %s adds a secret-shaped literal at %s:%d; build it at runtime and squash", short(h.Commit), h.File, h.Line)
}

// scanCommits finds the secret-shaped literals each commit in from..to adds.
func scanCommits(dir, from, to string) ([]secretHit, error) {
	out, err := gitx.Run(dir, "log", "--reverse", "--no-merges", "--no-color", "--no-renames", "--no-ext-diff",
		"--format=%x00commit %H", "-p", "--unified=0", from+".."+to)
	if err != nil {
		return nil, err
	}
	hits := scanPatch(out)
	for _, h := range gitleaksHits(dir, from, to) {
		if !slices.ContainsFunc(hits, func(o secretHit) bool { return o.Commit == h.Commit && o.File == h.File && o.Line == h.Line }) {
			hits = append(hits, h)
		}
	}
	return hits, nil
}

// scanPatch reads `git log -p --unified=0` output, each commit headed by a
// "\x00commit <sha>" line, and runs secretscan over its added lines.
func scanPatch(patch string) []secretHit {
	var hits []secretHit
	commit, file, line := "", "", 0
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "\x00commit "):
			commit, file = strings.TrimPrefix(l, "\x00commit "), ""
		case strings.HasPrefix(l, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(l, "+++ "), "b/")
			if file == "/dev/null" {
				file = ""
			}
		case strings.HasPrefix(l, "@@ "):
			// @@ -a,b +c,d @@: added lines start at c.
			if _, plus, ok := strings.Cut(l, " +"); ok {
				n, _, _ := strings.Cut(plus, " ")
				n, _, _ = strings.Cut(n, ",")
				line, _ = strconv.Atoi(n)
			}
		case strings.HasPrefix(l, "+") && file != "":
			if !strings.Contains(l, allowPragma) && len(secretscan.Find([]byte(l[1:]))) > 0 {
				hits = append(hits, secretHit{Commit: commit, File: file, Line: line})
			}
			line++
		}
	}
	return hits
}

// gitleaksBin is gitleaks on PATH, "" when it isn't installed.
var gitleaksBin = sync.OnceValue(func() string {
	p, _ := exec.LookPath("gitleaks")
	return p
})

// gitleaksHits runs gitleaks over from..to when it is installed. A run that
// fails or says nothing readable adds nothing: the patterns still ran.
func gitleaksHits(dir, from, to string) []secretHit {
	bin := gitleaksBin()
	if bin == "" {
		return nil
	}
	cmd := exec.Command(bin, "git", "--log-opts="+from+".."+to, "--redact", "--no-banner", "--exit-code", "0",
		"--report-format", "json", "--report-path", "-", "--log-level", "error", ".")
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		return nil
	}
	var found []struct {
		RuleID    string
		File      string
		Commit    string
		StartLine int
	}
	if json.Unmarshal(b, &found) != nil {
		return nil
	}
	var hits []secretHit
	for _, f := range found {
		hits = append(hits, secretHit{Commit: f.Commit, File: f.File, Line: f.StartLine, Rule: f.RuleID})
	}
	return hits
}

// secretGate is the land gate's history scan of the branch checked out in
// dir: each commit it adds over integration, not just its tree. It returns
// the message for the agent, "" when no commit adds a secret-shaped literal.
func (a *App) secretGate(dir string) string {
	if a.Cfg.CI.NoSecretGate {
		return ""
	}
	hits, err := scanCommits(dir, a.Cfg.Integration, "HEAD")
	if err != nil || len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Your branch could not land: a commit on it adds a secret-shaped literal. PR secret scanners (GitGuardian and the like) read every commit, not just the tree, so a later commit that removes it still leaves CI red.\n")
	for _, h := range hits {
		b.WriteString("- " + h.String() + "\n")
	}
	fmt.Fprintf(&b, "Rewrite your branch so no commit holds it: build the value at runtime (\"gh\" + \"p_…\"), then squash, e.g. `git reset --soft %s && git commit`. "+
		"Run the tests and call the saddle done tool again. A false positive can carry a `%s` comment on its line.", a.Cfg.Integration, allowPragma)
	return b.String()
}

// Events of the history-check repair, on the red task.
const (
	eventHistoryRewrite = "history_rewrite" // data: the layer's new head
	eventHistoryCard    = "history_card"    // data: why it can't be rewritten
	eventRepairRefused  = "ci_repair_refused"
)

// repairOfRepairErr refuses a spawn titled as a CI fix for a CI fix.
func repairOfRepairErr(title string) error {
	rest, ok := strings.CutPrefix(title, ciFixPrefix)
	if !ok {
		return nil
	}
	if _, about, _ := strings.Cut(rest, " "); strings.HasPrefix(about, ciFixPrefix) {
		return fmt.Errorf("spawn refused: %q would be a repair of a repair (#405); fold the fix into the original repair or escalate to the owner", title)
	}
	return nil
}

// ciFixPrefix starts every CI fix task's title (see ciFixTitle).
const ciFixPrefix = "Fix CI for "

// ciRepairOf names the task owner is a CI repair for, by title or by the
// ci-red state's repair records.
func (a *App) ciRepairOf(owner store.Task) (string, bool) {
	if rest, ok := strings.CutPrefix(owner.Title, ciFixPrefix); ok {
		orig, _, _ := strings.Cut(rest, " ")
		return orig, true
	}
	if s, err := a.CIRed(); err == nil {
		for _, r := range s.Repairs {
			if r.Repair == owner.ID {
				return r.Task, true
			}
		}
	}
	return "", false
}

// HistoryRepair runs before CIRedRepair on a red layer. It refuses to repair
// a repair, and handles a red history check: it rewrites the layer when
// saddle can, else raises one decision card. handled false leaves the layer
// to CIRedRepair.
func (a *App) HistoryRepair(task string, fails []CIRedFailure) (note string, handled bool, err error) {
	s, err := a.CIRed()
	if err != nil {
		return "", false, err
	}
	i := slices.IndexFunc(s.Red, func(l CIRedLayer) bool { return l.Task == task })
	if i < 0 {
		return "", false, nil
	}
	l := &s.Red[i]
	owner, err := a.Store.Task(task)
	if err != nil {
		owner = store.Task{ID: task}
	}
	if orig, ok := a.ciRepairOf(owner); ok {
		if l.Escalated {
			return "", true, nil
		}
		l.Escalated = true
		a.Store.Event(task, eventRepairRefused, orig)
		note = fmt.Sprintf("%s is itself a CI repair for %s, so no repair of a repair was spawned (#405).", task, orig)
		return note, true, errors.Join(a.SetCIRed(s), a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"CI is red on %s's PR %s (%s), and %s is itself a CI repair for %s. Saddle won't spawn a repair of a repair (#405). "+
				"Fold the fix into %s's layer yourself, `saddle unstack %s` to drop it, or `saddle sentinel ack` to stop it holding the layers above.",
			task, l.PR, strings.Join(l.Checks, ", "), task, orig, orig, task)))
	}

	var checks []string
	link := ""
	for _, f := range fails {
		if isHistoryCheck(f.Check, a.Cfg.CI.HistoryChecks) {
			checks = append(checks, f.Check)
			if link == "" {
				link = f.RunURL
			}
		}
	}
	for _, c := range l.Checks {
		if isHistoryCheck(c, a.Cfg.CI.HistoryChecks) && !slices.Contains(checks, c) {
			checks = append(checks, c)
		}
	}
	if len(checks) == 0 {
		return "", false, nil
	}
	if l.Escalated {
		return "", true, nil // the card is out; stop retrying
	}
	check := strings.Join(checks, ", ")

	var layer landedTask
	if stack, err := a.landedStack(); err == nil {
		if j := slices.IndexFunc(stack, func(x landedTask) bool { return x.ID == task }); j >= 0 {
			layer = stack[j]
		}
	}
	var notes []string
	if layer.From != "" && layer.Lost == "" {
		files, _ := gitx.ChangedFiles(a.Root, layer.From, layer.To)
		var ign []string
		for _, f := range files {
			if slices.Contains(scannerIgnoreFiles, f[strings.LastIndex(f, "/")+1:]) {
				ign = append(ign, f)
			}
		}
		if len(ign) > 0 {
			notes = append(notes, fmt.Sprintf("%s's layer adds %s, but %s is still red after the push, so the ignore file isn't a fix.",
				task, strings.Join(ign, ", "), check))
		}
	}
	card := func(why string) (string, bool, error) {
		l.Escalated = true
		a.Store.Event(task, eventHistoryCard, why)
		dismiss := "dismiss in dashboard: " + link
		if link == "" {
			dismiss = "dismiss it in the scanner's dashboard"
		}
		notes = append(notes, fmt.Sprintf("%s is a history check saddle can't clear by rewriting: %s. Saddle stops retrying it.", check, why))
		return strings.Join(notes, " "), true, errors.Join(a.SetCIRed(s), a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"Decision: %s is red on %s's PR %s at %s, and saddle can't rewrite its way out: %s. Saddle won't spawn fix tasks for it.\n"+
				"Recommended: %s. Or `saddle unstack %s` to drop the layer, or `saddle sentinel ack` to stop it holding the layers above (%s).",
			check, task, l.PR, short(l.Head), why, dismiss, task, strings.Join(l.Held, ", "))))
	}
	switch {
	case layer.From == "" || layer.Lost != "":
		return card(task + "'s layer isn't an unmerged saddle layer it can tell apart")
	case func() bool { last, ok := a.lastEvent(task, eventHistoryRewrite); return ok && last == l.Head }():
		return card(fmt.Sprintf("saddle already rewrote %s's layer to %s and the check is still red there", task, short(l.Head)))
	}
	// The tree itself may still hold the literal: then commits must fix it
	// first, and the ordinary repair does that; the next red head squashes.
	if tree, err := gitx.Run(a.Root, "diff", "--no-color", "--no-renames", "--unified=0", layer.From, layer.To); err == nil {
		if hits := scanPatch("\x00commit " + layer.To + "\n" + tree); len(hits) > 0 {
			return fmt.Sprintf("%s's tree still holds a secret-shaped literal at %s:%d, so a repair fixes the tree first; saddle squashes the layer once the check is red on its new head.",
				task, hits[0].File, hits[0].Line), false, nil
		}
	}
	n, _ := gitx.Run(a.Root, "rev-list", "--count", layer.From+".."+layer.To)
	if n == "1" {
		return card(fmt.Sprintf("%s's layer is one commit with a clean diff, so the finding is in a commit saddle doesn't own (merged, or someone else's)", task))
	}
	head, err := a.rewriteLayer(task, check)
	if err != nil {
		return card(fmt.Sprintf("rewriting %s's layer failed: %v", task, err))
	}
	a.Store.Event(task, eventHistoryRewrite, head)
	l.Repair = ""
	notes = append(notes, fmt.Sprintf("%s reads every commit, so saddle rewrote %s's layer instead of spawning a fix: its %s commits are squashed into %s (same tree), the layers above restacked, and the branch force-pushed with lease (#405).",
		check, task, n, short(head)))
	note = strings.Join(notes, " ")
	return note, true, errors.Join(a.SetCIRed(s), a.Notify(OrchestratorID, store.NoticeInfo, note))
}

// rewriteLayer squashes task's landed layer into one commit with the same
// tree, under the train lock, replays the layers after it onto that commit,
// moves the refs as restack does and pushes what moved. It returns the
// layer's new head.
func (a *App) rewriteLayer(task, check string) (string, error) {
	unlock, err := a.lockTrain()
	if err != nil {
		return "", err
	}
	defer unlock()
	stack, err := a.landedStack()
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(stack, func(l landedTask) bool { return l.ID == task })
	if i < 0 {
		return "", fmt.Errorf("%s isn't in the PR stack", task)
	}
	for _, l := range stack[i:] {
		if l.Lost != "" || l.From == "" {
			return "", fmt.Errorf("can't tell %s's landed work apart", l.ID)
		}
	}
	red := stack[i]
	msg := fmt.Sprintf("%s\n\nSquashed by saddle: %s reads every commit of a PR, so %s's commits were folded into one (#405).", red.Title, check, red.ID)
	head, err := trainGit(a.Root, "commit-tree", red.To+"^{tree}", "-p", red.From, "-m", msg)
	if err != nil {
		return "", err
	}
	integ, err := gitx.RevParse(a.Root, a.Cfg.Integration)
	if err != nil {
		return "", err
	}
	plan := []restacked{{landedTask: red, NewFrom: red.From, NewTo: head}}
	if above := stack[i+1:]; len(above) > 0 {
		moved, _, err := a.replay(above, head, map[string]bool{})
		if err != nil {
			return "", err
		}
		plan = append(plan, moved...)
	}
	var res RestackResult
	if err := a.moveStack(plan, integ, head, &res); err != nil {
		return "", err
	}
	a.Store.Event(red.ID, "restack", fmt.Sprintf("refs/heads/%s %s → %s (squashed for %s)", red.Branch, short(red.To), short(head), check))
	var full []restacked
	for _, l := range stack[:i] {
		full = append(full, restacked{landedTask: l, NewFrom: l.From, NewTo: l.To})
	}
	full = append(full, plan...)
	return head, a.republish(full, &res)
}
