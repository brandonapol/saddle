package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/remote"
	"github.com/brandonapol/saddle/internal/store"
)

// remoteRecent is how long after its last call a token counts as a
// connected controller in the header (#314).
const remoteRecent = 15 * time.Minute

// remoteHistory is how many events the first read looks back through for
// controllers seen before the TUI started.
const remoteHistory = 500

// remoteState is what the TUI knows of remote control: the event cursor
// and when each token was last seen.
type remoteState struct {
	cursor int64
	primed bool                 // the first read has seeded seen
	seen   map[string]time.Time // token name → its newest event
	order  []string             // token names, first seen first
}

// remoteRead is one refresh's kind=remote events, oldest first. History
// events predate the TUI: they seed the header but aren't replayed.
type remoteRead struct {
	events  []store.Event
	cursor  int64
	history bool
}

// readRemote reads the remote events after cursor. Unprimed, it reads the
// recent history instead and returns a cursor past it.
func readRemote(st *store.Store, cursor int64, primed bool) remoteRead {
	r := remoteRead{cursor: cursor, history: !primed}
	var es []store.Event
	var err error
	if primed {
		es, err = st.EventsSince(cursor)
	} else {
		es, err = st.Events(remoteHistory)
		if last, lerr := st.LastEventID(); lerr == nil {
			r.cursor = max(r.cursor, last)
		}
	}
	if err != nil {
		return remoteRead{cursor: cursor}
	}
	for _, e := range es {
		r.cursor = max(r.cursor, e.ID)
		if e.Kind == "remote" {
			r.events = append(r.events, e)
		}
	}
	return r
}

// parseRemote splits an audit line mirrored into the event log ("TOKEN
// DECISION TOOL from ADDR REASON") into its token, decision and tool. A
// failed login has no token; other remote events (serving, not serving)
// have none of the three.
func parseRemote(data string) (token, decision, tool string) {
	f := strings.Fields(data)
	isDecision := func(s string) bool {
		return s == remote.DecisionAllowed || s == remote.DecisionDenied || s == remote.DecisionPending
	}
	switch {
	case len(f) >= 1 && isDecision(f[0]):
		decision = f[0]
		if len(f) >= 2 && f[1] != "from" {
			tool = f[1]
		}
	case len(f) >= 2 && isDecision(f[1]):
		token, decision = f[0], f[1]
		if len(f) >= 3 && f[2] != "from" {
			tool = f[2]
		}
	}
	return token, decision, tool
}

// applyRemote notes the tokens r saw and, for new events, adds a chat event
// line for each one worth the owner's eye: everything but allowed read
// calls, which a polling controller makes all the time.
func (m *model) applyRemote(r remoteRead) {
	m.rc.cursor = max(m.rc.cursor, r.cursor)
	m.rc.primed = true
	if m.rc.seen == nil {
		m.rc.seen = map[string]time.Time{}
	}
	for _, e := range r.events {
		tok, dec, tool := parseRemote(e.Data)
		if tok != "" {
			if _, ok := m.rc.seen[tok]; !ok {
				m.rc.order = append(m.rc.order, tok)
			}
			if e.TS.After(m.rc.seen[tok]) {
				m.rc.seen[tok] = e.TS
			}
		}
		if r.history || dec == remote.DecisionAllowed && remote.ToolScopes[tool] == remote.ScopeRead {
			continue
		}
		line := "⇄ remote: " + strings.TrimSpace(e.Data)
		if m.app != nil && m.app.Store != nil {
			m.addChat(store.ChatEvent, line)
		} else {
			m.chat = append(m.chat, chatLine{role: store.ChatEvent, text: line})
		}
	}
}

// remoteControllers are the tokens seen within remoteRecent of now, in the
// order they first appeared.
func (m *model) remoteControllers(now time.Time) []string {
	var out []string
	for _, tok := range m.rc.order {
		if now.Sub(m.rc.seen[tok]) <= remoteRecent {
			out = append(out, tok)
		}
	}
	return out
}

// remoteSegment is the header's connected-controllers part, "" with none.
func (m *model) remoteSegment() string {
	toks := m.remoteControllers(time.Now())
	if len(toks) == 0 {
		return ""
	}
	return lipgloss.NewStyle().Foreground(cAccent).Render("⇄ " + strings.Join(toks, ", "))
}
