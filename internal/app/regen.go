package app

import (
	"fmt"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
)

// regenFor returns the regen entries that cover any of files, in config order.
func regenFor(regen []config.Regen, files []string) []config.Regen {
	var out []config.Regen
	for _, r := range regen {
		if slices.ContainsFunc(files, func(f string) bool { return regenMatch([]config.Regen{r}, f) }) {
			out = append(out, r)
		}
	}
	return out
}

// regenMatch reports whether file is a derived file some regen entry covers.
func regenMatch(regen []config.Regen, file string) bool {
	for _, r := range regen {
		for _, g := range r.Paths {
			if claims.Match(g, file) {
				return true
			}
		}
	}
	return false
}

// regenerate reruns the regen commands covering files in dir, after a rebase
// took integration's copy of them, and commits whatever they changed as the
// train. It returns the command output on failure.
func regenerate(dir string, regen []config.Regen, files []string) (string, error) {
	for _, r := range regenFor(regen, files) {
		if out, err := runShell(dir, r.Cmd); err != nil {
			return fmt.Sprintf("`%s` failed:\n%s", r.Cmd, tail(out, 40)), err
		}
	}
	if _, err := trainGit(dir, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := gitx.Run(dir, "diff", "--cached", "--quiet"); err == nil {
		return "", nil // integration's copy was already right
	}
	_, err := trainGit(dir, "commit", "-q", "--no-verify", "-m", "saddle: regenerate "+strings.Join(files, ", "))
	return "", err
}
