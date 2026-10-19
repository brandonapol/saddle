package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/procreap"
)

// Tmux is a private tmux server: its socket lives under Dir, which saddle
// finds through TMUX_TMPDIR, so tests never touch the user's tmux.
type Tmux struct {
	Dir string
	// Env is the environment tmux commands run with; a server started by one
	// of them gives it to every window. Empty means Dir alone over os env.
	Env []string
}

// maxSocketPath is the longest socket path that works everywhere: sun_path
// is 108 bytes on Linux and 104 on macOS, the NUL included.
const maxSocketPath = 103

// socket is the default server's socket path under Dir.
func (x *Tmux) socket() string {
	return filepath.Join(x.Dir, fmt.Sprintf("tmux-%d", os.Getuid()), "default")
}

// socketDir makes the socket dir under the first parent short enough for
// the socket path: the temp dir, then $XDG_RUNTIME_DIR (made for sockets),
// then /tmp. Under a deep TMPDIR, such as the train's gate dirs, the temp
// dir alone gives "File name too long".
func socketDir() (string, error) {
	var tooLong []string
	for _, p := range []string{os.TempDir(), os.Getenv("XDG_RUNTIME_DIR"), "/tmp"} {
		if p == "" {
			continue
		}
		dir, err := os.MkdirTemp(p, "e2etmux")
		if err != nil {
			continue
		}
		if x := (&Tmux{Dir: dir}); len(x.socket()) <= maxSocketPath {
			return dir, nil
		}
		_ = os.RemoveAll(dir)
		tooLong = append(tooLong, p)
	}
	return "", fmt.Errorf("no temp dir short enough for a tmux socket (tried %v)", tooLong)
}

// NewTmux makes a socket dir (short: socket paths are length-limited, see
// socketDir) and, when t ends, even failed, kills the server and every
// pane's process group (stopTmux). A journey killed before its cleanups
// run (go test's timeout) is reaped by Fork instead (#663).
func NewTmux(t testing.TB) *Tmux {
	t.Helper()
	dir, err := socketDir()
	if err != nil {
		t.Fatal(err)
	}
	x := &Tmux{Dir: dir}
	// tmux makes this dir for TMUX_TMPDIR but not for -S, which Run passes.
	if err := os.MkdirAll(filepath.Dir(x.socket()), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopTmux(x)
		_ = os.RemoveAll(dir)
	})
	return x
}

// stopTmux kills x's server and waits (up to 5s) for every pane's session
// to exit, then SIGKILLs what is left: the panes' process groups and the
// server itself. An agent's hook can outlive kill-server by a moment and
// reopen .saddle/state.db, recreating its -wal and -shm while t.TempDir is
// being removed ("directory not empty"); a pane that ignores the hangup
// would otherwise run on after the test (#663).
func stopTmux(x *Tmux) {
	panes, _ := x.Run("list-panes", "-a", "-F", "#{pane_pid}")
	servers, _ := x.Run("list-sessions", "-F", "#{pid}")
	_, _ = x.Run("kill-server")
	deadline := time.Now().Add(5 * time.Second)
	for _, f := range strings.Fields(panes) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid <= 1 {
			continue
		}
		for time.Now().Before(deadline) && exec.Command("pgrep", "-s", f).Run() == nil {
			time.Sleep(20 * time.Millisecond)
		}
		// A pane is its session's and its group's leader.
		if syscall.Kill(-pid, syscall.SIGKILL) == nil {
			for end := time.Now().Add(time.Second); time.Now().Before(end) && exec.Command("pgrep", "-s", f).Run() == nil; {
				time.Sleep(20 * time.Millisecond)
			}
		}
	}
	for _, f := range strings.Fields(servers) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
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
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, "TMUX=") })
	// Tag the server, and so its panes and fake agents, so a sweep finds
	// them by the tag even once their dir is gone (#692).
	if !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, procreap.EnvE2EDir+"=") }) {
		env = append(env, procreap.EnvE2EDir+"="+x.Dir)
	}
	return append(env, "TMUX_TMPDIR="+x.Dir)
}

// Run runs a tmux command against the private server. -S names its socket
// outright: tmux prefers $TMUX to TMUX_TMPDIR, so a stray TMUX in env would
// otherwise reach the user's own server.
func (x *Tmux) Run(args ...string) (string, error) {
	cmd := exec.Command("tmux", append([]string{"-S", x.socket()}, args...)...)
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
