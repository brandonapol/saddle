package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/store"
)

// The catch-up (#403, #412): alt+c lists what happened since the owner last
// looked, and every keystroke in the TUI counts as a look.

// awayGap is how long without a keystroke counts as having been away; the
// catch-up starts at the last key before such a gap.
const awayGap = 5 * time.Minute

// markEvery throttles writing the last look to disk while typing.
const markEvery = 30 * time.Second

// looks tracks the owner's keystrokes for the catch-up.
type looks struct {
	last   time.Time // the latest keystroke
	from   time.Time // where alt+c's catch-up starts
	marked time.Time // when the last look was last written
}

// looked records a keystroke at now. The first one of a session starts the
// catch-up at the look saddle saved last time (or CatchUpDefault ago); a
// keystroke after awayGap starts it at the key before the gap.
func (m *model) looked(now time.Time) {
	if m.app == nil || m.app.Store == nil {
		return
	}
	lk := &m.looks
	switch {
	case lk.last.IsZero():
		lk.from = m.app.CatchUpSince(now)
	case now.Sub(lk.last) > awayGap:
		lk.from = lk.last
	}
	lk.last = now
	if now.Sub(lk.marked) >= markEvery {
		if err := m.app.MarkLooked(now); err == nil {
			lk.marked = now
		}
	}
}

// catchUp shows the catch-up since the owner was last away in the chat.
func (m *model) catchUp() tea.Cmd {
	if m.app == nil || m.app.Store == nil {
		return flashCmd("catch-up needs saddle's state")
	}
	now := time.Now()
	from := m.looks.from
	if from.IsZero() {
		from = m.app.CatchUpSince(now)
	}
	bs, err := m.app.CatchUp(from, now)
	if err != nil {
		return flashCmd("catch-up: " + err.Error())
	}
	text := "Since " + from.Local().Format("Jan 2 15:04") + ":"
	for _, b := range bs {
		text += "\n• " + b
	}
	m.addChat(store.ChatEvent, strings.TrimSpace(text))
	m.follow = true
	return nil
}
