// Package orchrc runs saddle's orchestrator as an interactive Claude Code
// session with Remote Control on (saddle up --rc, #409), so the owner can
// steer it from the Claude mobile app or claude.ai/code.
//
// The session lives in its own window of the repo's tmux session. It gets
// the same settings, MCP config, model and permissions as the TUI's headless
// orchestrator, and a brief built on the plugin's (Brief). saddle up --rc
// holds the engine lock as app.LockUpRC, which leaves the orchestrator's
// notices to the plugin's wake paths: the saddle mod, a background `saddle
// plugin wait`, and the session's own hooks. Nothing ever types into the
// window: t0's task row records no window, so app.Notify and the idle
// waker have nothing to send keys to.
//
// The session id is chosen before the first launch and kept in
// .saddle/orchrc.json, so every later saddle up --rc resumes that session.
package orchrc

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
)

// stateFile is where the session is recorded, under .saddle/.
const stateFile = "orchrc.json"

// WindowName is the orchestrator's tmux window.
const WindowName = "t0-orchestrator"

// State is what saddle up --rc remembers between runs.
type State struct {
	// SessionID is the Claude Code session every run resumes.
	SessionID string `json:"session_id"`
	// Window is the tmux window the session runs in, while it runs.
	Window string `json:"window,omitempty"`
}

func statePath(root string) string { return filepath.Join(root, ".saddle", stateFile) }

// Load reads the recorded state; none recorded is the zero State.
func Load(root string) (State, error) {
	var st State
	b, err := os.ReadFile(statePath(root))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("%s: %w", statePath(root), err)
	}
	return st, nil
}

// Save records st.
func Save(root string, st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(root), append(b, '\n'), 0o644)
}

// Name is the session's Remote Control name, as the Claude app lists it.
func Name(root string) string { return "saddle: " + filepath.Base(root) }

// Session is a running Remote Control orchestrator.
type Session struct {
	State
	Name    string
	Resumed bool // the window's claude resumes an earlier conversation
	Reused  bool // the window was already running; nothing was started
	// Attach is a script that shows the window in a terminal (the TUI's
	// pane) without moving other clients of the tmux session.
	Attach string
}

// Launch builds the interactive launch: the headless orchestrator's launch,
// which carries its settings, MCP config, model and permissions, with the
// Remote Control brief and name. It resumes st's session when Claude Code
// has its transcript, and otherwise starts a session under that id with
// prompt as the first message. st gets a fresh id when it has none.
func Launch(a *app.App, st *State, prompt string, push bool) (agent.Launch, error) {
	if a.Cfg.Harness != "" && a.Cfg.Harness != config.HarnessClaude {
		return agent.Launch{}, fmt.Errorf("saddle up --rc needs Claude Code; harness is %q", a.Cfg.Harness)
	}
	l, _, err := a.Orchestrator()
	if err != nil {
		return agent.Launch{}, err
	}
	if l.Kind != agent.KindClaude {
		return agent.Launch{}, errors.New("saddle up --rc needs Claude Code")
	}
	if st.SessionID == "" {
		if st.SessionID, err = newUUID(); err != nil {
			return agent.Launch{}, err
		}
	}
	l.Brief = Brief(a, push)
	l.RemoteControl = Name(a.Root)
	l.Resume, l.SessionID, l.Prompt = "", "", ""
	if HasTranscript(l.Dir, st.SessionID) {
		l.Resume = st.SessionID
	} else {
		l.SessionID, l.Prompt = st.SessionID, prompt
	}
	return l, nil
}

// Start brings up the Remote Control orchestrator: it reuses the window a
// previous saddle up --rc left running, or opens a new one. The session id
// is recorded before claude starts.
func Start(a *app.App, prompt string, push bool) (Session, error) {
	st, err := Load(a.Root)
	if err != nil {
		return Session{}, err
	}
	s := Session{Name: Name(a.Root)}
	if st.Window != "" && a.Tmux.Alive(st.Window) && st.SessionID != "" {
		// Still the orchestrator's: refresh its task (no window recorded).
		if _, _, err := a.Orchestrator(); err != nil {
			return Session{}, err
		}
		s.State, s.Reused, s.Resumed = st, true, true
		s.Attach, err = writeAttach(a, st.Window)
		return s, err
	}
	l, err := Launch(a, &st, prompt, push)
	if err != nil {
		return Session{}, err
	}
	cmd, err := l.Write()
	if err != nil {
		return Session{}, err
	}
	st.Window = ""
	if err := Save(a.Root, st); err != nil {
		return Session{}, err
	}
	if a.Tmux.HasSession() {
		st.Window, err = a.Tmux.NewWindow(WindowName, a.Root, cmd)
	} else {
		st.Window, err = a.Tmux.NewSession(WindowName, a.Root, cmd)
	}
	if err != nil {
		return Session{}, err
	}
	if err := Save(a.Root, st); err != nil {
		return Session{}, err
	}
	a.Store.Event(app.OrchestratorID, "remote_control", fmt.Sprintf("session %s in window %s (resume %t)", st.SessionID, st.Window, l.Resume != ""))
	s.State, s.Resumed = st, l.Resume != ""
	s.Attach, err = writeAttach(a, st.Window)
	return s, err
}

// writeAttach writes the script the TUI's pane runs to show window: a
// throwaway tmux session grouped with saddle's, so selecting the window
// there doesn't switch anyone else's view, gone when the pane closes.
func writeAttach(a *app.App, window string) (string, error) {
	dir := filepath.Join(a.Root, ".saddle", "run", app.OrchestratorID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "attach-rc.sh")
	// $$ makes the grouped session's name differ per pane.
	sh := fmt.Sprintf(`#!/usr/bin/env bash
# Generated by saddle up --rc: shows the orchestrator's window.
unset TMUX
exec tmux new-session -t %s -s %s"$$" \; set-option destroy-unattached on \; select-window -t %s
`, shellQuote(a.Cfg.Session), shellQuote(a.Cfg.Session+"-rc-"), shellQuote(window))
	return p, os.WriteFile(p, []byte(sh), 0o755)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// unsafePath is what Claude Code replaces in a project dir to name its
// transcript folder.
var unsafePath = regexp.MustCompile(`[^A-Za-z0-9]`)

// TranscriptPath is where Claude Code keeps session id's conversation for a
// session started in dir.
func TranscriptPath(dir, id string) string {
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".claude")
	}
	return filepath.Join(base, "projects", unsafePath.ReplaceAllString(dir, "-"), id+".jsonl")
}

// HasTranscript reports whether Claude Code can resume session id in dir. A
// session started but never spoken to has none, and --resume would fail.
func HasTranscript(dir, id string) bool {
	_, err := os.Stat(TranscriptPath(dir, id))
	return err == nil
}

// newUUID returns a random (version 4) UUID, the form --session-id takes.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}
