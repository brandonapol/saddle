// Package refguard is a git reference-transaction hook that guards saddle's
// own branches. Only the merge train moves the integration branch, only a task
// (or the train) moves that task's branch, and a live task's branch can't be
// deleted. Every attempt, allowed or denied, is recorded in the events table.
//
// The hook lives in the repo's common hooks directory, so every worktree
// shares it. It runs `saddle refguard <state>`; only the "prepared" state can
// abort a transaction, so that is the only one it acts on.
package refguard

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// Event kinds in the events table. The event's task is the actor.
const (
	KindRef    = "ref"
	KindDenied = "ref_denied"
)

// Actors that aren't tasks.
const (
	Train   = "train"
	Unknown = "unknown"
)

// marker identifies a hook saddle wrote, so Install never clobbers anyone else's.
const marker = "# saddle refguard"

// Event is the data of a ref event.
type Event struct {
	Ref    string `json:"ref"`
	Old    string `json:"old"`
	New    string `json:"new"`
	Actor  string `json:"actor"`
	Denied string `json:"denied,omitempty"` // why the update was rejected
}

// Actor names who is moving refs: the merge train (SADDLE_TRAIN=1), else the
// task in SADDLE_TASK, else unknown. The train wins because it runs inside
// another task's process.
func Actor(getenv func(string) string) string {
	if getenv("SADDLE_TRAIN") == "1" {
		return Train
	}
	if t := getenv("SADDLE_TASK"); t != "" {
		return t
	}
	return Unknown
}

// Install writes the reference-transaction hook into the shared hooks
// directory of the repo at root. bin is the saddle binary the hook runs. A
// hook saddle didn't write is left alone and reported as an error.
func Install(root, bin string) error {
	hooks, err := gitx.Run(root, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		return err
	}
	path := filepath.Join(hooks, "reference-transaction")
	if b, err := os.ReadFile(path); err == nil && !strings.Contains(string(b), marker) {
		return fmt.Errorf("%s exists and was not written by saddle; chain `saddle refguard \"$@\"` from it to guard saddle's branches", path)
	}
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return err
	}
	script := "#!/bin/sh\n" + marker + ": only the merge train moves saddle's branches.\n" +
		"# Written by saddle; reinstalling overwrites it.\n" +
		"[ \"$1\" = prepared ] || exit 0\n" +
		"bin=" + shellQuote(bin) + "\n" +
		"# A missing binary must not block every ref update in the repo.\n" +
		"[ -x \"$bin\" ] || exit 0\n" +
		"exec \"$bin\" refguard \"$@\"\n"
	return os.WriteFile(path, []byte(script), 0o755)
}

type update struct{ old, new, ref string }

// Hook handles one reference-transaction invocation from the current
// directory: state is the hook's argument, r carries "<old> <new> <ref>"
// lines. A non-nil error aborts the transaction; its text is shown by git.
func Hook(state string, r io.Reader, getenv func(string) string) error {
	if state != "prepared" {
		return nil
	}
	cfg := config.Default()
	root, rootErr := gitx.Root(".")
	if rootErr == nil {
		if c, err := config.Load(root); err == nil {
			cfg = c
		}
	}
	var us []update
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 3 && guarded(f[2], cfg.Integration) {
			us = append(us, update{f[0], f[1], f[2]})
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(us) == 0 {
		return nil
	}

	// The store is only for liveness and the log; without it the rules still hold.
	var st *store.Store
	if rootErr == nil {
		if s, err := store.Open(filepath.Join(root, ".saddle", "state.db")); err == nil {
			st = s
			defer st.Close()
		}
	}
	live := func(task string) bool {
		if st == nil {
			return false // fail open: deletes are allowed when liveness is unknown
		}
		t, err := st.Task(task)
		return err == nil && t.Active()
	}

	actor := Actor(getenv)
	var denied []error
	for _, u := range us {
		// git passes a zero old value when the caller didn't pin one, so read
		// the ref (still unchanged while prepared) to tell a move from a create.
		cur, err := gitx.Run(".", "rev-parse", "--verify", "--quiet", u.ref)
		exists := err == nil
		if isZero(u.old) && exists {
			u.old = cur
		}
		e := Event{Ref: u.ref, Old: u.old, New: u.new, Actor: actor,
			Denied: check(actor, strings.TrimPrefix(u.ref, "refs/heads/"), cfg.Integration, isZero(u.new), exists, live)}
		kind := KindRef
		if e.Denied != "" {
			kind = KindDenied
			denied = append(denied, fmt.Errorf("[saddle] %s may not update %s: %s", actor, u.ref, e.Denied))
		}
		if st != nil {
			b, _ := json.Marshal(e)
			st.Event(actor, kind, string(b))
		}
	}
	return errors.Join(denied...)
}

// check returns why actor may not update branch, or "" to allow it.
func check(actor, branch, integration string, del, exists bool, live func(string) bool) string {
	if actor == Train {
		return ""
	}
	if branch == integration {
		if del || exists {
			return "only the merge train moves the integration branch"
		}
		return "" // created on first spawn
	}
	owner := Owner(branch)
	switch {
	case del && live(owner):
		return fmt.Sprintf("it belongs to %s, which is still live", owner)
	case del, !exists:
		return "" // spawn creates task branches; dead ones may be cleaned up
	case actor != owner:
		return fmt.Sprintf("it belongs to %s; only that task or the merge train moves it", owner)
	}
	return ""
}

// Owner returns the task a saddle branch belongs to: saddle/<task>-<slug>.
func Owner(branch string) string {
	name := strings.TrimPrefix(branch, "saddle/")
	if i := strings.IndexByte(name, '-'); i >= 0 {
		return name[:i]
	}
	return name
}

func guarded(ref, integration string) bool {
	return strings.HasPrefix(ref, "refs/heads/saddle/") || ref == "refs/heads/"+integration
}

// isZero reports whether oid is git's null object id (SHA-1 or SHA-256).
func isZero(oid string) bool { return strings.Trim(oid, "0") == "" }

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
