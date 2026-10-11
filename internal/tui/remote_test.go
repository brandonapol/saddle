package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// remoteEv is a kind=remote event as the audit mirror writes it.
func remoteEv(id int64, ts time.Time, data string) store.Event {
	return store.Event{ID: id, TS: ts, Task: "t0", Kind: "remote", Data: data}
}

func TestParseRemoteEvent(t *testing.T) {
	for _, c := range []struct {
		data, token, decision, tool string
	}{
		{"phone allowed status from 127.0.0.1:5555 ", "phone", "allowed", "status"},
		{"laptop pending spawn from 10.0.0.2:1 confirm code abc", "laptop", "pending", "spawn"},
		{" denied  from 127.0.0.1:5555 invalid, revoked or expired token", "", "denied", ""},
		{"ssh:box denied land from 1.2.3.4 scope", "ssh:box", "denied", "land"},
		{"remote control serving /repo at http://127.0.0.1:7777/mcp", "", "", ""},
	} {
		tok, dec, tool := parseRemote(c.data)
		if tok != c.token || dec != c.decision || tool != c.tool {
			t.Errorf("parseRemote(%q) = %q %q %q, want %q %q %q", c.data, tok, dec, tool, c.token, c.decision, c.tool)
		}
	}
}

// #314: the header names the tokens seen in the last few minutes, and no
// others; a failed login names none.
func TestRemoteHeaderShowsRecentControllers(t *testing.T) {
	m := newViewModel(200, 40)
	now := time.Now()
	m.applyRemote(remoteRead{history: true, events: []store.Event{
		remoteEv(1, now.Add(-2*time.Hour), "stale allowed status from 127.0.0.1:1 "),
		remoteEv(2, now.Add(-time.Minute), "phone allowed status from 127.0.0.1:1 "),
		remoteEv(3, now.Add(-30*time.Second), " denied  from 127.0.0.1:2 invalid, revoked or expired token"),
	}})
	m.applyRemote(remoteRead{events: []store.Event{remoteEv(4, now, "laptop allowed brief from 127.0.0.1:3 ")}})

	h := m.viewHeader()
	if !strings.Contains(h, "⇄ phone, laptop") {
		t.Fatalf("header lacks the controllers:\n%s", h)
	}
	if strings.Contains(h, "stale") {
		t.Errorf("header names a token not seen recently:\n%s", h)
	}

	m.rc.seen["phone"] = now.Add(-remoteRecent - time.Second)
	m.rc.seen["laptop"] = now.Add(-remoteRecent - time.Second)
	if h := m.viewHeader(); strings.Contains(h, "⇄") {
		t.Errorf("header shows controllers after they went quiet:\n%s", h)
	}
}

// #314: new remote events show in the chat's event lines, except allowed
// read calls (a polling controller would flood it); history read at start
// doesn't replay.
func TestRemoteEventsInChat(t *testing.T) {
	m := newViewModel(140, 40)
	now := time.Now()
	m.applyRemote(remoteRead{history: true, events: []store.Event{remoteEv(1, now, "old allowed kill from 127.0.0.1:1 ")}})
	m.applyRemote(remoteRead{events: []store.Event{
		remoteEv(2, now, "phone allowed status from 127.0.0.1:1 "),
		remoteEv(3, now, "phone allowed land from 127.0.0.1:1 "),
		remoteEv(4, now, "phone denied spawn from 127.0.0.1:1 scope admin needed"),
		remoteEv(5, now, "remote control serving /repo at http://127.0.0.1:7777/mcp"),
	}})
	var got []string
	for _, l := range m.chat {
		if l.role == store.ChatEvent {
			got = append(got, l.text)
		}
	}
	want := []string{
		"⇄ remote: phone allowed land from 127.0.0.1:1",
		"⇄ remote: phone denied spawn from 127.0.0.1:1 scope admin needed",
		"⇄ remote: remote control serving /repo at http://127.0.0.1:7777/mcp",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("chat events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The first read seeds from recent history; later reads return only new
// remote events.
func TestReadRemoteCursor(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	st.Event("t0", "remote", "phone allowed status from 127.0.0.1:1 ")
	st.Event("t1", "tool", "Bash")

	r := readRemote(st, 0, false)
	if !r.history || len(r.events) != 1 || r.cursor == 0 {
		t.Fatalf("first read = %+v, want one history event and a cursor", r)
	}
	st.Event("t0", "remote", "laptop allowed brief from 127.0.0.1:2 ")
	st.Event("t1", "tool", "Edit")
	r2 := readRemote(st, r.cursor, true)
	if r2.history || len(r2.events) != 1 || !strings.HasPrefix(r2.events[0].Data, "laptop") || r2.cursor <= r.cursor {
		t.Fatalf("second read = %+v, want only the laptop event", r2)
	}
	if r3 := readRemote(st, r2.cursor, true); len(r3.events) != 0 || r3.cursor != r2.cursor {
		t.Fatalf("third read = %+v, want nothing new", r3)
	}
}
