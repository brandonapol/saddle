// Package checkpoint keeps agent work from being lost between commits (#50).
//
// Every Policy.Every saddle snapshots each live worker's working tree,
// tracked and untracked files alike, into refs/saddle/checkpoints/<task>. The
// snapshot is built with a throwaway index, so the agent's index, HEAD and
// branch never move, and the ref sits outside refs/heads/, where the ref
// guard and every default push refspec look. Checkpoints are pruned when a
// task lands or is killed, and saddle never pushes them.
//
// The watcher also nudges agents through the mailbox to commit coherent
// units: once when a worktree has Policy.NudgeFiles dirty files, or has been
// dirty for Policy.NudgeAfter without a commit, and not again until the agent
// commits.
package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
)

// Prefix is the namespace checkpoints live in.
const Prefix = "refs/saddle/checkpoints/"

// Ref is the checkpoint ref of task.
func Ref(task string) string { return Prefix + task }

// ErrNone means a task has no checkpoint.
var ErrNone = errors.New("no checkpoint")

// Commit snapshots dir's working tree, untracked files included and ignored
// ones not, as a commit on top of HEAD with message msg. It uses a temporary
// index, so dir's own index and HEAD are untouched. It returns "" when dir is
// missing or clean.
func Commit(dir, msg string) (string, error) {
	tree, head, err := Tree(dir)
	if err != nil || tree == "" {
		return "", err
	}
	return commitTree(dir, tree, head, msg)
}

// Tree writes dir's working tree as a tree object and returns it with HEAD.
// The tree is "" when dir is missing or clean.
func Tree(dir string) (tree, head string, err error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", "", nil
	}
	dirty, err := gitx.Dirty(dir)
	if err != nil || len(dirty) == 0 {
		return "", "", err
	}
	tmp, err := os.MkdirTemp("", "saddle-checkpoint")
	if err != nil {
		return "", "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	g := scratch(dir, filepath.Join(tmp, "index"))
	if head, err = g("rev-parse", "HEAD"); err != nil {
		return "", "", err
	}
	if _, err := g("read-tree", head); err != nil {
		return "", "", err
	}
	if _, err := g("add", "-A"); err != nil {
		return "", "", err
	}
	tree, err = g("write-tree")
	return tree, head, err
}

func commitTree(dir, tree, head, msg string) (string, error) {
	return scratch(dir, "")("commit-tree", tree, "-p", head, "-m", msg)
}

// scratch runs git in dir with a fixed identity and, when index is set,
// GIT_INDEX_FILE pointing at it.
func scratch(dir, index string) func(args ...string) (string, error) {
	env := os.Environ()
	if index != "" {
		env = append(env, "GIT_INDEX_FILE="+index)
	}
	return func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=saddle", "-c", "user.email=saddle@localhost"}, args...)...)
		cmd.Dir, cmd.Env = dir, env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}
}

// Take checkpoints task's worktree dir into Ref(task) in the repo at root.
// It returns the new checkpoint, or "" when dir is missing, clean, or
// unchanged since the last checkpoint.
func Take(root, task, dir string) (string, error) {
	tree, head, err := Tree(dir)
	if err != nil || tree == "" {
		return "", err
	}
	if prev, err := Lookup(root, task); err == nil {
		same, _ := gitx.Run(root, "rev-parse", prev+"^{tree}", prev+"^")
		if same == tree+"\n"+head {
			return "", nil
		}
	}
	c, err := commitTree(dir, tree, head, "saddle checkpoint: work in progress of "+task)
	if err != nil {
		return "", err
	}
	if _, err := gitx.Run(root, "update-ref", "-m", "saddle checkpoint", Ref(task), c); err != nil {
		return "", err
	}
	return c, nil
}

// Lookup returns task's checkpoint commit, or ErrNone.
func Lookup(root, task string) (string, error) {
	c, err := gitx.Run(root, "rev-parse", "--verify", "--quiet", Ref(task)+"^{commit}")
	if err != nil || c == "" {
		return "", ErrNone
	}
	return c, nil
}

// List maps each task with a checkpoint to its commit.
func List(root string) (map[string]string, error) {
	out, err := gitx.Run(root, "for-each-ref", "--format=%(refname) %(objectname)", Prefix)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if ref, c, ok := strings.Cut(line, " "); ok {
			m[strings.TrimPrefix(ref, Prefix)] = c
		}
	}
	return m, nil
}

// Prune deletes task's checkpoint. A task without one is fine.
func Prune(root, task string) error {
	if _, err := Lookup(root, task); err != nil {
		return nil
	}
	_, err := gitx.Run(root, "update-ref", "-d", Ref(task))
	return err
}

// dirtyFiles lists dir's changed files, each file in a new directory on
// its own line.
func dirtyFiles(dir string) ([]string, error) {
	out, err := gitx.Run(dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// Target is a worktree the watcher checkpoints.
type Target struct{ Task, Worktree string }

// Host is what the watcher needs from saddle.
type Host interface {
	// Targets lists the live workers to checkpoint.
	Targets() ([]Target, error)
	// Nudge sends task a [saddle] message through its mailbox.
	Nudge(task, text string) error
	// Event records something that happened.
	Event(task, kind, detail string)
}

// Policy says how often to checkpoint and when to nudge. A zero field turns
// its rule off.
type Policy struct {
	Every      time.Duration // between checkpoints of one worktree
	NudgeFiles int           // dirty files that earn a nudge to commit
	NudgeAfter time.Duration // time dirty without a commit that earns one
}

// DefaultPolicy checkpoints every minute and nudges at eight dirty files or
// twenty minutes without a commit.
var DefaultPolicy = Policy{Every: time.Minute, NudgeFiles: 8, NudgeAfter: 20 * time.Minute}

type state struct {
	head       string
	last       time.Time // last checkpoint attempt
	dirtySince time.Time
	nudged     bool
}

// Watcher checkpoints and nudges every Target on each Tick.
type Watcher struct {
	root   string
	host   Host
	policy Policy
	now    func() time.Time
	state  map[string]*state
}

func NewWatcher(root string, host Host, p Policy) *Watcher {
	return &Watcher{root: root, host: host, policy: p, now: time.Now, state: map[string]*state{}}
}

// Run ticks until ctx ends, every Policy.Every or every minute when that
// is off. Failures are events.
func (w *Watcher) Run(ctx context.Context) {
	every := w.policy.Every
	if every <= 0 {
		every = time.Minute
	}
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		w.Tick()
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}

// Tick checkpoints every target due one and nudges those that have earned it.
func (w *Watcher) Tick() {
	ts, err := w.host.Targets()
	if err != nil {
		w.host.Event("", "checkpoint_error", err.Error())
		return
	}
	seen := map[string]bool{}
	for _, t := range ts {
		seen[t.Task] = true
		w.tick(t)
	}
	for id := range w.state {
		if !seen[id] {
			delete(w.state, id)
		}
	}
}

func (w *Watcher) tick(t Target) {
	if fi, err := os.Stat(t.Worktree); err != nil || !fi.IsDir() {
		return
	}
	st := w.state[t.Task]
	if st == nil {
		st = &state{}
		w.state[t.Task] = st
	}
	now := w.now()
	head, err := gitx.Run(t.Worktree, "rev-parse", "HEAD")
	if err != nil {
		w.host.Event(t.Task, "checkpoint_error", err.Error())
		return
	}
	if head != st.head {
		st.head, st.dirtySince, st.nudged = head, time.Time{}, false
	}
	dirty, err := dirtyFiles(t.Worktree)
	if err != nil {
		w.host.Event(t.Task, "checkpoint_error", err.Error())
		return
	}
	if len(dirty) == 0 {
		st.dirtySince, st.nudged = time.Time{}, false
		return
	}
	if st.dirtySince.IsZero() {
		st.dirtySince = now
	}
	if w.policy.Every > 0 && (st.last.IsZero() || now.Sub(st.last) >= w.policy.Every) {
		st.last = now
		if _, err := Take(w.root, t.Task, t.Worktree); err != nil {
			w.host.Event(t.Task, "checkpoint_failed", err.Error())
		}
	}
	if st.nudged {
		return
	}
	var why string
	switch {
	case w.policy.NudgeFiles > 0 && len(dirty) >= w.policy.NudgeFiles:
		why = fmt.Sprintf("%d files are uncommitted", len(dirty))
	case w.policy.NudgeAfter > 0 && now.Sub(st.dirtySince) >= w.policy.NudgeAfter:
		why = fmt.Sprintf("your changes have been uncommitted for %s", now.Sub(st.dirtySince).Round(time.Minute))
	default:
		return
	}
	st.nudged = true
	text := why + ". Commit a small, coherent unit of finished work now on your branch, then carry on. Saddle checkpoints your work in progress, but only commits land."
	if err := w.host.Nudge(t.Task, text); err != nil {
		w.host.Event(t.Task, "checkpoint_error", err.Error())
		return
	}
	w.host.Event(t.Task, "commit_nudge", why)
}
