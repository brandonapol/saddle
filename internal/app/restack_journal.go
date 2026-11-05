package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

const RestackJournalFile = "restack-journal.json"

// EventRestackAutoAborted records a journal whose writer died rolled back
// on its own at the next train-lock acquire (#777).
const EventRestackAutoAborted = "restack_auto_aborted"

type restackJournal struct {
	// Pid is the process that wrote the journal; 0 in journals written
	// before it was recorded.
	Pid       int           `json:"pid,omitempty"`
	Plan      []restacked   `json:"plan"`
	Moves     []RestackMove `json:"moves"`
	Checkouts []store.Task  `json:"checkouts,omitempty"`
}

func (a *App) pendingRestack() error {
	_, err := os.Stat(a.stateDir(RestackJournalFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("interrupted restack: run saddle restack --continue to complete it or saddle restack --abort to restore its old refs")
}

func (a *App) writeRestackJournal(j restackJournal) error {
	if err := a.pendingRestack(); err != nil {
		return err
	}
	if j.Pid == 0 {
		j.Pid = os.Getpid()
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(a.stateDir(), "restack-journal-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), a.stateDir(RestackJournalFile)); err != nil {
		return err
	}
	dir, err := os.Open(a.stateDir())
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// moveRestackRefs commits every compare-and-swap in a single Git transaction.
// A rejected ref, hook, or interrupted prepare leaves all refs unchanged.
func (a *App) moveRestackRefs(moves []RestackMove, abort bool) error {
	var input strings.Builder
	input.WriteString("start\n")
	for _, m := range moves {
		old, next := m.Old, m.New
		if abort {
			old, next = next, old
		}
		fmt.Fprintf(&input, "update %s %s %s\n", m.Ref, next, old)
	}
	input.WriteString("prepare\ncommit\n")
	cmd := exec.Command("git", "-C", a.Root, "update-ref", "-m", "saddle: atomic restack", "--stdin")
	cmd.Env = append(os.Environ(), "SADDLE_TRAIN=1")
	cmd.Stdin = strings.NewReader(input.String())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restack ref transaction failed; no refs moved: %w: %s; run saddle restack --abort or --continue", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// applyRestackJournal completes j, or with abort rolls it back. Abort never
// fails on a dirty checkout: it leaves that checkout (and, while it is on its
// branch, that branch's ref) alone and returns it in skipped (#777).
func (a *App) applyRestackJournal(j restackJournal, abort bool) (skipped []string, err error) {
	dirty := func(task store.Task) bool {
		d, err := gitx.Dirty(task.Worktree)
		return err != nil || len(d) > 0
	}
	skip := func(task store.Task) {
		if !slices.Contains(skipped, task.Worktree) {
			skipped = append(skipped, task.Worktree)
			a.Store.Event(task.ID, "restack_abort", "left dirty checkout "+task.Worktree+" alone")
		}
	}
	// Detach clean task checkouts first. If interrupted after the transaction,
	// their files and index still match their detached HEAD, never a moved ref.
	keep := map[string]bool{} // refs left alone under a dirty checkout
	for _, task := range j.Checkouts {
		if !a.checkedOut(task) {
			continue
		}
		if dirty(task) {
			if !abort {
				return nil, fmt.Errorf("%s has uncommitted changes; preserve them before resuming restack", task.Worktree)
			}
			skip(task)
			keep["refs/heads/"+task.Branch] = true
			continue
		}
		if _, err := trainGit(task.Worktree, "checkout", "-q", "--detach"); err != nil {
			return nil, err
		}
	}
	var pending []RestackMove
	for _, m := range j.Moves {
		current, err := gitx.RevParse(a.Root, m.Ref)
		if err != nil {
			return nil, err
		}
		if current != m.Old && current != m.New {
			if !abort {
				return nil, fmt.Errorf("%s changed outside the restack transaction; run saddle restack --abort to leave that change intact and discard this plan", m.Ref)
			}
			a.Store.Event(m.Task, "restack_abort", "preserved externally changed "+m.Ref)
			continue
		}
		if keep[m.Ref] {
			continue
		}
		if !abort && current == m.Old || abort && current == m.New {
			pending = append(pending, m)
		}
	}
	if len(pending) > 0 {
		if err := a.moveRestackRefs(pending, abort); err != nil {
			return nil, err
		}
	}
	for _, r := range j.Plan {
		state, note := store.TrainOK, r.NewFrom+".."+r.NewTo
		if r.gone() {
			state, note = TrainMerged, r.rangeNote()
		}
		if abort {
			state, note = r.State, r.rangeNote()
		}
		if err := a.Store.SetTrain(r.ID, state, note, false); err != nil {
			return nil, err
		}
	}
	for _, task := range j.Checkouts {
		if _, err := os.Stat(task.Worktree); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if a.checkedOut(task) {
			continue
		}
		if dirty(task) {
			if !abort {
				return nil, fmt.Errorf("%s has uncommitted changes; preserve them before resuming restack", task.Worktree)
			}
			skip(task) // stays detached at the commit its files match
			continue
		}
		if _, err := trainGit(task.Worktree, "checkout", "-q", task.Branch); err != nil {
			return nil, err
		}
	}
	for _, m := range j.Moves {
		a.Store.Event(m.Task, "restack", fmt.Sprintf("%s %s → %s (abort=%t)", m.Ref, short(m.Old), short(m.New), abort))
	}
	return skipped, os.Remove(a.stateDir(RestackJournalFile))
}

// readRestackJournal reads and checks the journal; ok is false when there is
// none.
func (a *App) readRestackJournal() (j restackJournal, ok bool, err error) {
	b, err := os.ReadFile(a.stateDir(RestackJournalFile))
	if errors.Is(err, os.ErrNotExist) {
		return j, false, nil
	}
	if err != nil {
		return j, false, err
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, false, err
	}
	for _, m := range j.Moves {
		if !strings.HasPrefix(m.Ref, "refs/heads/") || strings.ContainsAny(m.Ref, "\r\n \x00") || !validJournalOID(m.Old) || !validJournalOID(m.New) {
			return j, false, errors.New("invalid restack journal ref")
		}
	}
	return j, true, nil
}

// recoverDeadRestack rolls back a journal whose writer is dead, so a crash or
// a stolen lock mid-restack never needs a human (#777). Every move is a CAS,
// so restoring old tips is safe: a ref someone moved since is kept. The
// caller holds the train lock. A journal whose writer still runs is left for
// it; if the rollback itself fails, the journal stays and pendingRestack
// reports it.
func (a *App) recoverDeadRestack() {
	j, ok, err := a.readRestackJournal()
	if err != nil || !ok || j.Pid != 0 && pidAlive(j.Pid) {
		return
	}
	skipped, err := a.applyRestackJournal(j, true)
	if err != nil {
		a.Store.Event("", "restack_auto_abort_failed", fmt.Sprintf("journal from dead pid %d: %v", j.Pid, err))
		return
	}
	note := fmt.Sprintf("rolled back the restack journal of dead pid %d", j.Pid)
	if len(skipped) > 0 {
		note += "; left dirty checkouts alone: " + strings.Join(skipped, ", ")
	}
	a.Store.Event("", EventRestackAutoAborted, note)
}

// abortFailedRestack rolls back j after its own apply failed, so a live
// process (the daemon) never leaves a journal behind it (#777).
func (a *App) abortFailedRestack(j restackJournal, cause error) error {
	if _, err := a.applyRestackJournal(j, true); err != nil {
		return errors.Join(cause, fmt.Errorf("rolling the restack back failed too: %w", err))
	}
	return cause
}

// RecoverRestack completes or rolls back the journaled local transaction.
// Remote publishing can be retried with prs after recovery. skipped lists
// the dirty checkouts an abort left alone.
func (a *App) RecoverRestack(abort bool) (skipped []string, err error) {
	unlock, err := a.lockTrainForRecovery()
	if err != nil {
		return nil, err
	}
	defer unlock()
	j, ok, err := a.readRestackJournal()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("no interrupted restack to recover")
	}
	return a.applyRestackJournal(j, abort)
}

func validJournalOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// RestackInterrupted reports the journal without opening or changing the repo.
func RestackInterrupted(root string) bool {
	_, err := os.Stat(filepath.Join(root, ".saddle", RestackJournalFile))
	return err == nil
}
