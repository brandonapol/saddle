// Package refguard is a git reference-transaction hook that guards saddle's
// own branches. Only the merge train moves the integration branch, only a task
// (or the train) moves that task's branch, and a live task's branch can't be
// deleted. Every attempt is recorded in the events table.
package refguard

import "io"

// Install writes the reference-transaction hook into the repo at root.
func Install(root, bin string) error { return nil }

// Hook handles one reference-transaction invocation.
func Hook(state string, r io.Reader, getenv func(string) string) error { return nil }

const (
	KindRef    = "ref"
	KindDenied = "ref_denied"
)

type Event struct {
	Ref, Old, New, Actor, Denied string
}
