package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/automerge"
)

// fakeBatcher records what the batch key wrote.
type fakeBatcher struct {
	output string
	sets   []string
}

func (f *fakeBatcher) BatchMode() bool { return f.output == "batch" }

func (f *fakeBatcher) SetConfig(key, value string) (string, error) {
	f.sets = append(f.sets, key+"="+value)
	f.output = value
	return key + " = " + value, nil
}

// #913: alt+b flips between one batch stack and individual PRs from any
// view, turns auto-merge on with batch mode so the batch merges, and the
// header says so.
func TestBatchKeyTogglesOutput(t *testing.T) {
	m := newViewModel(140, 40)
	f := &fakeBatcher{output: "per-task"}
	am := &fakeAutomerger{}
	m.bter, m.amer = f, am
	m.am = &automerge.Status{}

	if strings.Contains(m.viewHeader(), "batch") {
		t.Fatalf("header shows batch mode while off:\n%s", m.viewHeader())
	}
	runKey(m, altKey('b')) // chat focused: alt+b must not type a "b"
	if !slices.Equal(f.sets, []string{"train.output=batch"}) {
		t.Fatalf("sets = %v, want train.output=batch", f.sets)
	}
	if strings.Contains(m.input.Value(), "b") {
		t.Errorf("alt+b typed into the chat: %q", m.input.Value())
	}
	if !slices.Equal(am.calls, []string{"on"}) {
		t.Errorf("auto-merge calls = %q, want on", am.calls)
	}
	if h := m.viewHeader(); !strings.Contains(h, "▤ batch") {
		t.Fatalf("header lacks the indicator:\n%s", h)
	}

	m.Update(altKey('3'))
	m.am.Enabled = true
	runKey(m, altKey('b'))
	if !slices.Equal(f.sets, []string{"train.output=batch", "train.output=per-task"}) {
		t.Fatalf("sets = %v, want batch then per-task", f.sets)
	}
	if len(am.calls) != 1 {
		t.Errorf("turning batch off touched auto-merge: %q", am.calls)
	}
	if h := m.viewHeader(); strings.Contains(h, "batch") {
		t.Errorf("header still shows batch mode:\n%s", h)
	}
}
