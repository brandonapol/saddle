// Package tmux drives the tmux CLI: one session per repo, one window per agent.
package tmux

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
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

func run(args ...string) (string, error) {
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
	return run("new-session", "-d", "-s", t.Session, "-n", window, "-c", dir, "-P", "-F", "#{window_id}", cmd)
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
	out, err := run("list-windows", "-a", "-F", "#{window_id}")
	if err != nil {
		return false
	}
	for _, w := range strings.Split(out, "\n") {
		if w == id {
			return true
		}
	}
	return false
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
