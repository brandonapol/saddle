package hook

import (
	"path/filepath"
	"strings"
)

// BypassesHooks returns why a shell command skips the repo's own git hooks,
// or "" when it doesn't: git commit --no-verify or -n, git push --no-verify,
// or a -c core.hooksPath override (#212). Agents must pass the repo's
// pre-commit gate like its developers do. Text inside quotes, other
// programs' arguments and pathspecs after -- don't count.
func BypassesHooks(cmd string) string {
	for _, seg := range segments(cmd) {
		if why := gitBypass(seg); why != "" {
			return why
		}
	}
	return ""
}

// gitBypass checks one simple command's words.
func gitBypass(w []string) string {
	w = w[assignments(w):] // VAR=value prefixes
	if len(w) == 0 || filepath.Base(w[0]) != "git" {
		return ""
	}
	i := 1
	for ; i < len(w) && strings.HasPrefix(w[i], "-"); i++ {
		switch w[i] {
		case "-C", "--git-dir", "--work-tree", "--namespace", "--exec-path", "--config-env":
			i++
		case "-c":
			if i+1 < len(w) && strings.HasPrefix(strings.ToLower(w[i+1]), "core.hookspath=") {
				return "git -c core.hooksPath=... skips the repo's own hooks"
			}
			i++
		}
	}
	if i >= len(w) {
		return ""
	}
	args := w[i+1:]
	switch w[i] {
	case "commit":
		return commitBypass(args)
	case "push":
		for _, a := range args {
			if a == "--" {
				break
			}
			if a == "--no-verify" {
				return "git push --no-verify skips the repo's pre-push hook"
			}
		}
	}
	return ""
}

// commitValued are git commit's long options that take their value as the
// next word.
var commitValued = map[string]bool{
	"--message": true, "--file": true, "--reuse-message": true, "--reedit-message": true,
	"--template": true, "--author": true, "--date": true, "--cleanup": true,
	"--fixup": true, "--squash": true, "--trailer": true, "--pathspec-from-file": true,
}

func commitBypass(args []string) string {
	const why = "git commit --no-verify (-n) skips the repo's pre-commit and commit-msg hooks"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return ""
		case a == "--no-verify":
			return why
		case strings.HasPrefix(a, "--"):
			if commitValued[a] {
				i++
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for j, c := range a[1:] {
				if c == 'n' {
					return why
				}
				if strings.ContainsRune("mFcCt", c) {
					if j+2 == len(a) {
						i++ // the value is the next word
					}
					break
				}
				if c == 'S' {
					break // an optional key id runs to the end of the word
				}
			}
		}
	}
	return ""
}
