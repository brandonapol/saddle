// Package refguard is a git reference-transaction hook that guards saddle's
// own branches. Only the merge train moves the integration branch, only a task
// (or the train) moves that task's branch, and a live task's branch can't be
// deleted. Every attempt, allowed or denied, is recorded in the events table.
//
// The hook lives in the repo's common hooks directory, so every worktree
// shares it. It runs `saddle refguard <state>`; only the "prepared" state can
// abort a transaction, so that is the only one it acts on.
//
// Pushes never run reference-transaction, so a pre-push hook beside it runs
// `saddle refguard pre-push`: only the train pushes saddle's branches.
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
	"github.com/brandonapol/saddle/internal/lintgate"
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
	Remote string `json:"remote,omitempty"` // set for pushes
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

// Install writes the reference-transaction and pre-push hooks into the shared
// hooks directory of the repo at root. bin is the saddle binary they run.
//
// A hook saddle didn't write is chained, never clobbered: it moves to
// <name>.pre-saddle (a symlink stays a symlink, so a repo's tracked hook is
// never written through) and saddle's hook runs it after its own check, with
// the same arguments and stdin; its failure still blocks (#212). When that
// slot is taken too, nothing is written and the error says so. Then it
// installs wrappers for the hooks the repo ships (see InstallRepoHooks).
func Install(root, bin string) error {
	hooks, err := hooksDir(root)
	if err != nil {
		return err
	}
	scripts := []struct{ name, why, guard, run string }{
		{"reference-transaction", "only the merge train moves saddle's branches",
			`[ "$1" = prepared ] && `,
			`"$bin" refguard "$@"`},
		// The remote rides in the environment so the command keeps the
		// `refguard <state>` shape that test binaries answer.
		{"pre-push", "only the merge train pushes saddle's branches", "",
			`SADDLE_PUSH_REMOTE="$1" "$bin" refguard pre-push`},
	}
	var chain []string
	for _, sc := range scripts {
		path := filepath.Join(hooks, sc.name)
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), marker) {
			continue
		}
		if _, err := os.Lstat(path + lintgate.ChainSuffix); err == nil {
			return fmt.Errorf("%s exists and was not written by saddle, and %s is taken, so saddle can't chain it; merge them, then run saddle init again",
				path, path+lintgate.ChainSuffix)
		}
		chain = append(chain, path)
	}
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		return err
	}
	for _, path := range chain {
		if err := os.Rename(path, path+lintgate.ChainSuffix); err != nil {
			return err
		}
	}
	for _, sc := range scripts {
		path := filepath.Join(hooks, sc.name)
		body := "#!/bin/sh\n" + marker + ": " + sc.why + ".\n" +
			"# Written by saddle; reinstalling overwrites it. A repo hook that was\n" +
			"# here lives at " + filepath.Base(path) + lintgate.ChainSuffix + " and runs after saddle's check.\n" +
			"bin=" + shellQuote(bin) + "\n" +
			"orig=" + shellQuote(path+lintgate.ChainSuffix) + "\n" +
			"input=$(cat)\n" +
			"feed() { [ -z \"$input\" ] || printf '%s\\n' \"$input\"; }\n" +
			"# A missing binary must not block every ref update or push in the repo.\n" +
			sc.guard + "[ -x \"$bin\" ] && { feed | " + sc.run + " || exit 1; }\n" +
			"[ -x \"$orig\" ] || exit 0\n" +
			"feed | \"$orig\" \"$@\"\n"
		// Remove first: never write through a symlink into someone else's file.
		_ = os.Remove(path)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			return err
		}
	}
	// The repo's own hooks run in every worktree too (#223).
	_, err = InstallRepoHooks(root)
	return err
}

func hooksDir(root string) (string, error) {
	return gitx.Run(root, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
}

// HookState is what is installed at one of the hooks Install writes.
type HookState struct {
	Name    string // reference-transaction or pre-push
	Path    string
	Present bool   // a file exists at Path
	Saddle  bool   // saddle wrote it
	Bin     string // the saddle binary it runs, when Saddle
	// Chained is the repo hook saddle's hook runs after its own check, when
	// one is parked at <Path>.pre-saddle.
	Chained string
}

// Installed reports the state of the reference-transaction and pre-push
// hooks in the repo at root, in that order.
func Installed(root string) ([]HookState, error) {
	hooks, err := hooksDir(root)
	if err != nil {
		return nil, err
	}
	var out []HookState
	for _, name := range []string{"reference-transaction", "pre-push"} {
		h := HookState{Name: name, Path: filepath.Join(hooks, name)}
		if b, err := os.ReadFile(h.Path); err == nil {
			h.Present = true
			h.Saddle = strings.Contains(string(b), marker)
			if h.Saddle {
				h.Bin = hookBin(string(b))
			}
		}
		if _, err := os.Lstat(h.Path + lintgate.ChainSuffix); err == nil {
			h.Chained = h.Path + lintgate.ChainSuffix
		}
		out = append(out, h)
	}
	return out, nil
}

// hookBin reads back the bin=<shellQuote(bin)> line Install writes.
func hookBin(script string) string {
	for _, line := range strings.Split(script, "\n") {
		if q, ok := strings.CutPrefix(line, "bin="); ok && len(q) >= 2 {
			return strings.ReplaceAll(q[1:len(q)-1], `'\''`, "'")
		}
	}
	return ""
}

type update struct{ old, new, ref string }

// Hook handles one reference-transaction invocation from the current
// directory: state is the hook's argument, r carries "<old> <new> <ref>"
// lines. A non-nil error aborts the transaction; its text is shown by git.
// State "pre-push" comes from the pre-push hook instead; see Push.
func Hook(state string, r io.Reader, getenv func(string) string) error {
	if state == "pre-push" {
		return Push(getenv("SADDLE_PUSH_REMOTE"), r, getenv)
	}
	if state != "prepared" {
		return nil
	}
	cfg, root, rootErr := load()
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
	st := openStore(root, rootErr)
	if st != nil {
		defer func() { _ = st.Close() }()
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
		if exists && u.old == u.new {
			continue // checking out a branch rewrites it in place; nothing moves
		}
		e := Event{Ref: u.ref, Old: u.old, New: u.new, Actor: actor,
			Denied: check(actor, strings.TrimPrefix(u.ref, "refs/heads/"), cfg.Integration, isZero(u.new), exists, live)}
		record(st, e)
		if e.Denied != "" {
			denied = append(denied, fmt.Errorf("[saddle] %s may not update %s: %s", actor, u.ref, e.Denied))
		}
	}
	return errors.Join(denied...)
}

// Push handles one pre-push invocation from the current directory: remote is
// the remote's name, r carries "<local ref> <local sha> <remote ref> <remote
// sha>" lines. Only the train pushes saddle's branches, even a task's own; a
// non-nil error aborts the whole push.
func Push(remote string, r io.Reader, getenv func(string) string) error {
	cfg, root, rootErr := load()
	var es []Event
	actor := Actor(getenv)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 4 || !guarded(f[2], cfg.Integration) {
			continue
		}
		e := Event{Ref: f[2], Old: f[3], New: f[1], Actor: actor, Remote: remote}
		if actor != Train {
			e.Denied = "only the merge train pushes saddle's branches"
		}
		es = append(es, e)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(es) == 0 {
		return nil
	}
	st := openStore(root, rootErr)
	if st != nil {
		defer func() { _ = st.Close() }()
	}
	var denied []error
	for _, e := range es {
		record(st, e)
		if e.Denied != "" {
			denied = append(denied, fmt.Errorf("[saddle] %s may not push %s: %s", actor, e.Ref, e.Denied))
		}
	}
	return errors.Join(denied...)
}

// load finds the repo from the current directory and its config, falling
// back to the defaults.
func load() (config.Config, string, error) {
	cfg := config.Default()
	root, err := gitx.Root(".")
	if err == nil {
		if c, err := config.Load(root); err == nil {
			cfg = c
		}
	}
	return cfg, root, err
}

func openStore(root string, rootErr error) *store.Store {
	if rootErr != nil {
		return nil
	}
	st, err := store.Open(filepath.Join(root, ".saddle", "state.db"))
	if err != nil {
		return nil
	}
	return st
}

// record logs e under its actor; a nil store logs nothing.
func record(st *store.Store, e Event) {
	if st == nil {
		return
	}
	kind := KindRef
	if e.Denied != "" {
		kind = KindDenied
	}
	b, _ := json.Marshal(e)
	st.Event(e.Actor, kind, string(b))
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
