package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/store"
)

// AdapterName is the agent a task was launched with, e.g. "claude".
func (a *App) AdapterName(t store.Task) string { return a.taskAdapter(t).Name() }

// AskOwner sends task's question about path to the task that claims it, or
// to the orchestrator for serial files, which the merge train owns. It
// returns who was asked.
func (a *App) AskOwner(task, path, question string) (string, error) {
	path = claims.Clean(path)
	if path == "" || strings.TrimSpace(question) == "" {
		return "", errors.New("ask_owner: give a path and a question")
	}
	all, err := a.Store.Claims()
	if err != nil {
		return "", err
	}
	owner, glob := claims.Owner(all, task, path)
	if owner == "" {
		for _, own := range all[task] {
			if claims.Match(own, path) {
				return "", fmt.Errorf("you own %s (claim %s); nobody else to ask", path, own)
			}
		}
		for _, g := range a.Cfg.Serial {
			if claims.Match(g, path) {
				owner, glob = OrchestratorID, "serial "+g
				break
			}
		}
	}
	if owner == "" {
		return "", fmt.Errorf("nobody claims %s; claim it yourself if you need it", path)
	}
	msg := fmt.Sprintf("Question from %s about %s (%s): %s\nAnswer with the saddle message tool to %s.", task, path, glob, question, task)
	if err := a.Notify(owner, store.NoticeAction, msg); err != nil {
		return "", err
	}
	a.Store.Event(task, "ask", owner+" "+path)
	return owner, nil
}
