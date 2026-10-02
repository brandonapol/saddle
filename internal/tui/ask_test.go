package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/narrator"
	"github.com/brandonapol/saddle/internal/store"
)

type fakeAsker struct {
	qs     []narrator.Question
	answer string
	err    error
}

func (f *fakeAsker) Ask(_ context.Context, q narrator.Question) (narrator.Line, error) {
	f.qs = append(f.qs, q)
	return narrator.Line{Task: q.Task, Text: f.answer}, f.err
}

func newAskModel(t *testing.T) (*model, *fakeAsker) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := newViewModel(120, 40)
	m.app.Store = st
	f := &fakeAsker{answer: "t1 is editing the view router."}
	m.asker = f
	m.capture = func(task string, lines int) (string, error) {
		return task + " screen tail", nil
	}
	return m, f
}

func typeIn(m *model, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// run feeds a command's message back into the model, as the program does.
func feed(m *model, c tea.Cmd) {
	if c != nil {
		m.Update(c())
	}
}

// alt+a turns the chat input into a question for the narrator; the question
// and its answer are threaded into the chat log and the store.
func TestAskNarratorThreadsQuestionAndAnswer(t *testing.T) {
	m, f := newAskModel(t)
	m.Update(altKey('a'))
	if !m.asking() || !strings.Contains(m.viewChat(60, 20), "ASK NARRATOR") {
		t.Fatal("alt+a should aim the chat at the narrator")
	}
	typeIn(m, "what is t1 doing?")
	_, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.asking() {
		t.Error("after sending, the chat goes back to the orchestrator")
	}
	feed(m, c)
	if len(f.qs) != 1 || f.qs[0].Text != "what is t1 doing?" || f.qs[0].Screen != "" || f.qs[0].Task != "" {
		t.Fatalf("questions = %+v", f.qs)
	}
	n := len(m.chat)
	if n < 2 || m.chat[n-2].text != "» what is t1 doing?" || m.chat[n-1].text != "↳ t1 is editing the view router." {
		t.Fatalf("chat tail = %+v", m.chat[max(n-2, 0):])
	}
	hist, _ := m.app.Store.Chat(10)
	if len(hist) != 2 || hist[0].Role != store.ChatNarrator || hist[1].Role != store.ChatNarrator {
		t.Errorf("stored = %+v", hist)
	}
}

// The selected agent's screen goes along only when the user opts in, for
// that one question.
func TestAskNarratorScreenIsOptIn(t *testing.T) {
	m, f := newAskModel(t)
	m.Update(altKey('a'))
	m.Update(altKey('s'))
	if !strings.Contains(m.viewChat(60, 20), "t1's screen") {
		t.Errorf("the box should say whose screen goes along:\n%s", m.viewChat(60, 20))
	}
	typeIn(m, "why is it slow?")
	_, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	feed(m, c)
	if len(f.qs) != 1 || f.qs[0].Task != "t1" || f.qs[0].Screen != "t1 screen tail" {
		t.Fatalf("question = %+v", f.qs)
	}
	if !strings.Contains(m.chat[len(m.chat)-2].text, "with t1's screen") {
		t.Errorf("the question line should note the screen: %q", m.chat[len(m.chat)-2].text)
	}
	// The next question starts without the screen.
	m.Update(altKey('a'))
	typeIn(m, "and now?")
	_, c = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	feed(m, c)
	if f.qs[1].Screen != "" {
		t.Errorf("opt-in leaked into the next question: %+v", f.qs[1])
	}
}

func TestAskNarratorErrorsAndOff(t *testing.T) {
	m, f := newAskModel(t)
	f.err = narrator.ErrCapReached
	m.Update(altKey('a'))
	typeIn(m, "status?")
	_, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	feed(m, c)
	if last := m.chat[len(m.chat)-1].text; !strings.HasPrefix(last, "↳ ") || !strings.Contains(last, "cap") {
		t.Errorf("a refused question should say why: %q", last)
	}
	// esc goes back to the orchestrator without asking.
	m.Update(altKey('a'))
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.asking() {
		t.Error("esc should leave ask mode")
	}

	m.asker = nil
	_, c = m.Update(altKey('a'))
	if m.asking() {
		t.Fatal("with the narrator off there is no one to ask")
	}
	if c == nil {
		t.Fatal("alt+a with the narrator off should say why")
	}
	if msg, ok := c().(flashMsg); !ok || !strings.Contains(string(msg), "narrator is off") {
		t.Errorf("flash = %v", msg)
	}
}

// Questions and answers render distinctly and fit narrow chats.
func TestAskLinesRender(t *testing.T) {
	for _, text := range []string{"» what is t1 doing with the very long title here?", "↳ t1 is editing the view router and will land soon, then t2."} {
		for _, w := range []int{30, 40, 120} {
			out := renderLine(chatLine{role: store.ChatNarrator, text: text}, w, lipgloss.NewStyle().Width(w-2))
			for _, l := range strings.Split(out, "\n") {
				if lipgloss.Width(l) > w {
					t.Errorf("width %d: %q too wide", w, l)
				}
			}
			if !strings.Contains(out, text[:4]) {
				t.Errorf("width %d: marker lost: %q", w, out)
			}
		}
	}
}
