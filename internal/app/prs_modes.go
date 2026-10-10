package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
)

type PRsOptions struct{ DryRun, PushOnly, GateOnly bool }

type PRsLayer struct {
	Task   string `json:"task"`
	Title  string `json:"title"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	Head   string `json:"head"`
	Recut  bool   `json:"recut"`
	Group  int    `json:"group"`
}

type PRsPlan struct {
	Layers []PRsLayer  `json:"layers"`
	Checks []GateCheck `json:"checks"`
	Note   string      `json:"note"`
}

func (a *App) preview() *App {
	return &App{Root: a.Root, Cfg: a.Cfg, Store: a.Store, Bin: a.Bin, previewLayout: true}
}

// PreviewPRs reads the local landing and cached base snapshot. It neither
// reconciles state nor invokes repository hooks, gates, pushes or GitHub writes.
// Replayed commits are computed as unreachable objects, without moving refs.
func (a *App) PreviewPRs() (PRsPlan, error) {
	plan := PRsPlan{Checks: a.GateChecks(false), Note: "Local landing and cached base snapshot; no fetch, reconciliation, gate, push or PR update."}
	if setup := strings.TrimSpace(a.Cfg.Train.Prepublish.Setup); setup != "" && len(plan.Checks) > 0 {
		plan.Checks = append([]GateCheck{{Name: "setup", Cmd: setup}}, plan.Checks...)
	}
	stack, err := a.landedStack()
	if err != nil {
		return plan, err
	}
	if len(stack) == 0 {
		return plan, errors.New("nothing has landed yet")
	}
	layout, order, err := a.preview().stackLayout(stack)
	if err != nil {
		return plan, err
	}
	for _, i := range order {
		l := stack[i]
		plan.Layers = append(plan.Layers, PRsLayer{Task: l.ID, Title: l.Title, Branch: l.Branch, Base: a.prBase(stack, layout, i), Head: layout[i].Head, Recut: layout[i].Head != l.To, Group: layout[i].Group})
	}
	return plan, nil
}

// PRsWithOptions retains publication's gates and holds for push-only, and
// runs just the gate against a ref-free layout for gate-only.
func (a *App) PRsWithOptions(options PRsOptions) ([]string, error) {
	n := 0
	for _, on := range []bool{options.DryRun, options.PushOnly, options.GateOnly} {
		if on {
			n++
		}
	}
	if n > 1 {
		return nil, errors.New("choose only one of --dry-run, --push-only and --gate-only")
	}
	if options.DryRun {
		return nil, errors.New("use PreviewPRs to read the dry-run plan")
	}
	if options.GateOnly {
		unlock, err := a.lockTrain()
		if err != nil {
			return nil, err
		}
		defer unlock()
		stack, err := a.landedStack()
		if err != nil {
			return nil, err
		}
		if len(stack) == 0 {
			return nil, errors.New("nothing has landed yet")
		}
		layout, order, err := a.preview().stackLayout(stack)
		if err != nil {
			return nil, err
		}
		_, err = a.prGate(stack, layout, order, func(int) bool { return true })
		return nil, err
	}
	result, err := a.publishMode(options.PushOnly)
	return result.urls, err
}

func (a *App) replayLayout(dir, tip, from, to string) (string, bool, error) {
	if !a.previewLayout {
		return replayOnto(dir, tip, from, to)
	}
	commits, err := gitx.Run(a.Root, "rev-list", "--reverse", "--no-merges", from+".."+to)
	if err != nil {
		return "", false, err
	}
	for _, commit := range strings.Fields(commits) {
		tree, err := gitx.Run(a.Root, "-c", "merge.directoryRenames=true", "-c", "merge.renames=true", "merge-tree", "--write-tree", "--no-messages", "--merge-base="+commit+"^", tip, commit)
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return "", false, nil // merge conflict: use the linear stack
			}
			return "", false, err
		}
		current, err := gitx.Run(a.Root, "rev-parse", tip+"^{tree}")
		if err != nil {
			return "", false, err
		}
		if tree == current {
			continue
		}
		meta, err := gitx.Run(a.Root, "log", "-1", "--format=%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI", commit)
		if err != nil {
			return "", false, err
		}
		who := strings.Split(meta, "\x00")
		if len(who) != 6 {
			return "", false, fmt.Errorf("invalid commit metadata for %s", commit)
		}
		raw, err := exec.Command("git", "-C", a.Root, "cat-file", "commit", commit).Output()
		if err != nil {
			return "", false, err
		}
		_, message, ok := strings.Cut(string(raw), "\n\n")
		if !ok {
			return "", false, fmt.Errorf("missing commit message for %s", commit)
		}
		cmd := exec.Command("git", "-C", a.Root, "commit-tree", tree, "-p", tip)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME="+who[0], "GIT_AUTHOR_EMAIL="+who[1], "GIT_AUTHOR_DATE="+who[2], "GIT_COMMITTER_NAME="+who[3], "GIT_COMMITTER_EMAIL="+who[4], "GIT_COMMITTER_DATE="+who[5])
		cmd.Stdin = strings.NewReader(message)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", false, fmt.Errorf("preview commit: %w: %s", err, out)
		}
		tip = strings.TrimSpace(string(out))
	}
	return tip, true, nil
}
