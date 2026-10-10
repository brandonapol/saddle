package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

const RestackJournalFile = "restack-journal.json"

type restackJournal struct {
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

func (a *App) applyRestackJournal(j restackJournal, abort bool) error {
	var pending []RestackMove
	for _, m := range j.Moves {
		current, err := gitx.RevParse(a.Root, m.Ref)
		if err != nil {
			return err
		}
		if current != m.Old && current != m.New {
			if !abort {
				return fmt.Errorf("%s changed outside the restack transaction; run saddle restack --abort to leave that change intact and discard this plan", m.Ref)
			}
			a.Store.Event(m.Task, "restack_abort", "preserved externally changed "+m.Ref)
			continue
		}
		if !abort && current == m.Old || abort && current == m.New {
			pending = append(pending, m)
		}
	}
	// Detach clean task checkouts first. If interrupted after the transaction,
	// their files and index still match their detached HEAD, never a moved ref.
	for _, task := range j.Checkouts {
		if !a.checkedOut(task) {
			continue
		}
		if dirty, err := gitx.Dirty(task.Worktree); err != nil || len(dirty) > 0 {
			return fmt.Errorf("%s has uncommitted changes; preserve them before resuming restack", task.Worktree)
		}
		if _, err := trainGit(task.Worktree, "checkout", "-q", "--detach"); err != nil {
			return err
		}
	}
	if len(pending) > 0 {
		if err := a.moveRestackRefs(pending, abort); err != nil {
			return err
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
			return err
		}
	}
	for _, task := range j.Checkouts {
		if _, err := os.Stat(task.Worktree); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if a.checkedOut(task) {
			continue
		}
		if dirty, err := gitx.Dirty(task.Worktree); err != nil || len(dirty) > 0 {
			return fmt.Errorf("%s has uncommitted changes; preserve them before resuming restack", task.Worktree)
		}
		if _, err := trainGit(task.Worktree, "checkout", "-q", task.Branch); err != nil {
			return err
		}
	}
	for _, m := range j.Moves {
		a.Store.Event(m.Task, "restack", fmt.Sprintf("%s %s → %s (abort=%t)", m.Ref, short(m.Old), short(m.New), abort))
	}
	return os.Remove(a.stateDir(RestackJournalFile))
}

// RecoverRestack completes or rolls back the journaled local transaction.
// Remote publishing can be retried with prs after recovery.
func (a *App) RecoverRestack(abort bool) error {
	unlock, err := a.lockTrainForRecovery()
	if err != nil {
		return err
	}
	defer unlock()
	b, err := os.ReadFile(a.stateDir(RestackJournalFile))
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no interrupted restack to recover")
	}
	if err != nil {
		return err
	}
	var j restackJournal
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	for _, m := range j.Moves {
		if !strings.HasPrefix(m.Ref, "refs/heads/") || strings.ContainsAny(m.Ref, "\r\n \x00") || !validJournalOID(m.Old) || !validJournalOID(m.New) {
			return errors.New("invalid restack journal ref")
		}
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
