package planner

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// maxChurn caps the churn list in a snapshot.
const maxChurn = 20

// TakeSnapshot reads the repo at root with git: tracked files collapsed to
// depth path segments, CODEOWNERS, and the files most changed in the last
// 200 commits. Serial is left for the caller to fill from config.
func TakeSnapshot(ctx context.Context, root string, depth int) (Snapshot, error) {
	if depth <= 0 {
		depth = 3
	}
	files, err := gitLines(ctx, root, "ls-files")
	if err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{Tree: collapse(files, depth)}
	for _, p := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		if b, err := os.ReadFile(filepath.Join(root, p)); err == nil {
			s.Codeowners = string(b)
			break
		}
	}
	// Churn is best effort: a repo with no commits has none.
	if log, err := gitLines(ctx, root, "log", "-n", "200", "--name-only", "--format="); err == nil {
		count := map[string]int{}
		for _, p := range log {
			if p != "" {
				count[p]++
			}
		}
		for p, n := range count {
			s.Churn = append(s.Churn, Churn{p, n})
		}
		slices.SortFunc(s.Churn, func(a, b Churn) int {
			if a.Commits != b.Commits {
				return b.Commits - a.Commits
			}
			return strings.Compare(a.Path, b.Path)
		})
		if len(s.Churn) > maxChurn {
			s.Churn = s.Churn[:maxChurn]
		}
	}
	return s, nil
}

// collapse lists files with at most depth segments as-is and folds deeper
// ones into "dir/ (N files)" entries for their depth-segment directory.
func collapse(files []string, depth int) []string {
	dirs := map[string]int{}
	var out []string
	for _, f := range files {
		parts := strings.Split(f, "/")
		if len(parts) <= depth {
			out = append(out, f)
			continue
		}
		dirs[strings.Join(parts[:depth], "/")+"/"]++
	}
	for d, n := range dirs {
		out = append(out, fmt.Sprintf("%s (%d files)", d, n))
	}
	slices.Sort(out)
	return out
}

func gitLines(ctx context.Context, dir string, args ...string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, nil
}
