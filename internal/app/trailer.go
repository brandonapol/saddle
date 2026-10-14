package app

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/refguard"
)

// Every task commit carries a Saddle-Task trailer (refguard's commit-msg
// hook) and every PR body ends with one, so a squash merge carries it into
// base too. "Is this task's work in base?" is then answered by identity
// first, which survives squashes, reviewer edits and rewordings that
// patch-ids and content can't follow (#331); patch-id and content stay the
// fallback for work from before the trailer.

// taskTrailerOn reports whether ref has a commit since doesn't, whose
// message names task in a Saddle-Task line, made no earlier than since's
// newest work. The line may sit anywhere in the message: GitHub's squash
// appends Co-authored-by paragraphs after the PR body. A merge older than
// since's last commit can't vouch for it: that work came after.
func (a *App) taskTrailerOn(task, ref, since string) bool {
	if !taskIDRe.MatchString(task) || taskIDRe.FindString(task) != task {
		return false
	}
	pat := "^" + refguard.TaskTrailer + ":[[:space:]]*" + regexp.QuoteMeta(task) + "[[:space:]]*$"
	out, err := gitx.Run(a.Root, "log", "-1", "--format=%ct", "-E", "--grep="+pat, since+".."+ref)
	if err != nil || out == "" {
		return false
	}
	at, err := gitx.Run(a.Root, "log", "-1", "--format=%at", since)
	if err != nil {
		return false
	}
	merged, err1 := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	last, err2 := strconv.ParseInt(strings.TrimSpace(at), 10, 64)
	return err1 == nil && err2 == nil && merged >= last
}

// withTaskTrailer ends a PR body with task's Saddle-Task line, unless it
// already has one, so the squash commit GitHub makes from it names the task.
func withTaskTrailer(body, task string) string {
	line := refguard.TaskTrailer + ": " + task
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) == line {
			return body
		}
	}
	body = strings.TrimRight(body, "\n")
	if body != "" {
		body += "\n\n"
	}
	return body + line + "\n"
}
