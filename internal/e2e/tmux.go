package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Tmux is a private tmux server: its socket lives under Dir, which saddle
// finds through TMUX_TMPDIR, so tests never touch the user's tmux.
type Tmux struct {
	Dir string
	// Env is the environment tmux commands run with; a server started by one
	// of them gives it to every window. Empty means Dir alone over os env.
	Env []string
}

// NewTmux makes a socket dir (short, under the system temp dir: socket
// paths are length-limited) and kills the server when t ends.
func NewTmux(t testing.TB) *Tmux {
	t.Helper()
	dir, err := os.MkdirTemp("", "e2etmux")
	if err != nil {
		t.Fatal(err)
	}
	x := &Tmux{Dir: dir}
	t.Cleanup(func() {
		_, _ = x.Run("kill-server")
		_ = os.RemoveAll(dir)
	})
	return x
}

func (x *Tmux) env() []string {
	env := x.Env
	if len(env) == 0 {
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "TMUX=") && !strings.HasPrefix(kv, "TMUX_TMPDIR=") {
				env = append(env, kv)
			}
		}
	}
	return append(env, "TMUX_TMPDIR="+x.Dir)
}

// Run runs a tmux command against the private server.
func (x *Tmux) Run(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	cmd.Env = x.env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

// NewSession starts a detached session of width x height running cmd in dir.
func (x *Tmux) NewSession(name string, width, height int, dir, cmd string) error {
	_, err := x.Run("new-session", "-d", "-s", name, "-x", itoa(width), "-y", itoa(height), "-c", dir, cmd)
	return err
}

// Capture is the visible screen of target, one line per row.
func (x *Tmux) Capture(target string) (string, error) {
	return x.Run("capture-pane", "-p", "-t", target)
}

// SendKeys sends tmux key names (Enter, Escape, C-c, Down…).
func (x *Tmux) SendKeys(target string, keys ...string) error {
	_, err := x.Run(append([]string{"send-keys", "-t", target}, keys...)...)
	return err
}

// Type types text literally.
func (x *Tmux) Type(target, text string) error {
	_, err := x.Run("send-keys", "-t", target, "-l", text)
	return err
}

// Resize sets target's window size.
func (x *Tmux) Resize(target string, width, height int) error {
	_, err := x.Run("resize-window", "-t", target, "-x", itoa(width), "-y", itoa(height))
	return err
}

// Window is one tmux window.
type Window struct {
	Session, ID, Name string
	Dead              bool
}

// Windows lists every window on the server; none when it isn't running.
func (x *Tmux) Windows() []Window {
	out, err := x.Run("list-windows", "-a", "-F", "#{session_name}\t#{window_id}\t#{window_name}\t#{pane_dead}")
	if err != nil {
		return nil
	}
	var ws []Window
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(l, "\t")
		if len(f) == 4 {
			ws = append(ws, Window{Session: f[0], ID: f[1], Name: f[2], Dead: f[3] == "1"})
		}
	}
	return ws
}

// Alive reports whether window id exists.
func (x *Tmux) Alive(id string) bool {
	for _, w := range x.Windows() {
		if w.ID == id {
			return true
		}
	}
	return false
}
