// Package tmux drives the tmux CLI: one session per repo, one window per agent.
package tmux

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Driver is what saddle needs from a terminal multiplexer. Tests use a fake.
type Driver interface {
	HasSession() bool
	NewSession(window, dir, cmd string) (string, error)
	NewWindow(name, dir, cmd string) (string, error)
	KillWindow(id string) error
	Alive(id string) bool
	SendText(id, text string) error
	SendKeys(id string, keys ...string) error
	Capture(id string, lines int) (string, error)
	KillSession() error
}

type Tmux struct{ Session string }

var run = func(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (t Tmux) HasSession() bool {
	_, err := run("has-session", "-t", "="+t.Session)
	return err == nil
}

// NewSession creates the session detached, with its first window running cmd.
func (t Tmux) NewSession(window, dir, cmd string) (string, error) {
	id, err := run("new-session", "-d", "-s", t.Session, "-n", window, "-c", dir, "-P", "-F", "#{window_id}", cmd)
	if err != nil {
		return id, err
	}
	// Copy-on-select is a nicety; a failure here must not lose the session.
	_ = t.configureSession(clipboardCommand())
	return id, nil
}

// clipboardCommand returns the system clipboard tool, or "" to fall back to
// tmux's own set-clipboard (OSC52).
func clipboardCommand() string {
	if p, err := exec.LookPath("wl-copy"); err == nil {
		return p
	}
	return ""
}

// configureSession makes dragging with the mouse select text and copy it to
// the system clipboard on release. Options are scoped to this session. Key
// tables are server-wide in tmux, so the drag-end binding is guarded on the
// session name and falls back to tmux's stock behaviour for every other session.
func (t Tmux) configureSession(clip string) error {
	target := "=" + t.Session
	if _, err := run("set-option", "-t", target, "mouse", "on"); err != nil {
		return err
	}
	copyCmd := "send-keys -X copy-pipe-and-cancel"
	if clip != "" {
		copyCmd += " " + clip
	} else if _, err := run("set-option", "-s", "set-clipboard", "on"); err != nil {
		// No wl-copy: tmux sets the clipboard itself via OSC52.
		return err
	}
	guard := "#{==:#{session_name}," + t.Session + "}"
	for _, table := range []string{"copy-mode", "copy-mode-vi"} {
		if _, err := run("bind-key", "-T", table, "MouseDragEnd1Pane",
			"if-shell", "-F", guard, copyCmd, "send-keys -X copy-pipe-and-cancel"); err != nil {
			return err
		}
	}
	return nil
}

// NewWindow opens a background window at the end of the session and returns its id (@N).
func (t Tmux) NewWindow(name, dir, cmd string) (string, error) {
	return run("new-window", "-d", "-t", t.Session+":", "-n", name, "-c", dir, "-P", "-F", "#{window_id}", cmd)
}

func (t Tmux) KillWindow(id string) error {
	_, err := run("kill-window", "-t", id)
	return err
}

func (t Tmux) Alive(id string) bool {
	ws, err := t.Windows()
	return err == nil && ws[id]
}

// Windows returns the id of every window on the server, from one tmux call.
func (t Tmux) Windows() (map[string]bool, error) {
	out, err := run("list-windows", "-a", "-F", "#{window_id}")
	if err != nil {
		return nil, err
	}
	ws := map[string]bool{}
	for _, w := range strings.Split(out, "\n") {
		if w != "" {
			ws[w] = true
		}
	}
	return ws, nil
}

// CaptureMany captures the last lines[id] lines of each window's pane in
// one tmux call, a marker line between them. It fails as a whole if any
// window is gone; callers check Windows first.
func (t Tmux) CaptureMany(lines map[string]int) (map[string]string, error) {
	out := map[string]string{}
	if len(lines) == 0 {
		return out, nil
	}
	mark := fmt.Sprintf("saddle-pane-%d-", time.Now().UnixNano())
	var args []string
	for id, n := range lines {
		if len(args) > 0 {
			args = append(args, ";")
		}
		args = append(args, "display-message", "-p", mark+id, ";",
			"capture-pane", "-p", "-t", id, "-S", fmt.Sprintf("-%d", n))
	}
	text, err := run(args...)
	if err != nil {
		return nil, err
	}
	id := ""
	var cur []string
	flush := func() {
		if id != "" {
			out[id] = strings.TrimRight(strings.Join(cur, "\n"), "\n ")
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if next, ok := strings.CutPrefix(l, mark); ok {
			flush()
			id, cur = next, nil
			continue
		}
		cur = append(cur, l)
	}
	flush()
	return out, nil
}

// SendText types text into the window's active pane and presses Enter.
func (t Tmux) SendText(id, text string) error {
	if _, err := run("send-keys", "-t", id, "-l", text); err != nil {
		return err
	}
	_, err := run("send-keys", "-t", id, "Enter")
	return err
}

// SendKeys sends tmux key names (Enter, Escape, Down, "1"…) without typing them literally.
func (t Tmux) SendKeys(id string, keys ...string) error {
	_, err := run(append([]string{"send-keys", "-t", id}, keys...)...)
	return err
}

func (t Tmux) KillSession() error {
	_, err := run("kill-session", "-t", "="+t.Session)
	return err
}

func (t Tmux) Capture(id string, lines int) (string, error) {
	return run("capture-pane", "-p", "-t", id, "-S", fmt.Sprintf("-%d", lines))
}

// WindowName returns the window's name, which saddle sets to <task>-<slug>.
func (t Tmux) WindowName(id string) (string, error) {
	return run("display-message", "-p", "-t", id, "#{window_name}")
}

// CaptureStyled is Capture with SGR escapes kept (capture-pane -e).
func (t Tmux) CaptureStyled(id string, lines int) (string, error) {
	return run("capture-pane", "-e", "-p", "-t", id, "-S", fmt.Sprintf("-%d", lines))
}
