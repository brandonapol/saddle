// Package termpane runs a shell in a pseudo-terminal and emulates the
// terminal it draws on, so a TUI can embed an interactive shell pane.
package termpane

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ErrExited is returned when sending input to a shell that has exited.
var ErrExited = errors.New("shell exited")

// Term is a shell running in a pty, with the screen it has drawn.
type Term struct {
	mu     sync.Mutex
	screen *Screen
	scroll int // lines scrolled back into history; 0 follows the output

	pty  *os.File
	cmd  *exec.Cmd
	pid  int
	once sync.Once

	updates chan struct{}
	done    chan struct{}
}

// Start runs shell in dir on a new pty of the given size.
func Start(shell, dir string, cols, rows int) (*Term, error) {
	cols, rows = max(cols, 1), max(rows, 1)
	ptm, ttyName, err := openPty()
	if err != nil {
		return nil, err
	}
	tty, err := os.OpenFile(ttyName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptm.Close()
		return nil, err
	}
	defer tty.Close()
	if err := setSize(ptm, cols, rows); err != nil {
		ptm.Close()
		return nil, err
	}
	cmd := exec.Command(shell)
	cmd.Dir = dir
	cmd.Env = termEnv()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		ptm.Close()
		return nil, err
	}
	t := &Term{
		screen:  NewScreen(cols, rows),
		pty:     ptm,
		cmd:     cmd,
		pid:     cmd.Process.Pid,
		updates: make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	go t.read()
	go t.wait()
	return t, nil
}

// termEnv is the environment with TERM describing what Screen emulates.
func termEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TERM=") {
			env = append(env, e)
		}
	}
	return append(env, "TERM=xterm-256color")
}

func (t *Term) read() {
	buf := make([]byte, 32*1024)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			t.mu.Lock()
			t.screen.Feed(buf[:n])
			replies := t.screen.TakeReplies()
			t.mu.Unlock()
			if len(replies) > 0 {
				_, _ = t.pty.Write(replies)
			}
			t.notify()
		}
		if err != nil {
			return
		}
	}
}

func (t *Term) wait() {
	_ = t.cmd.Wait()
	close(t.done)
	t.closePty()
	t.notify()
}

func (t *Term) notify() {
	select {
	case t.updates <- struct{}{}:
	default:
	}
}

func (t *Term) closePty() { t.once.Do(func() { t.pty.Close() }) }

// Updates receives a value (coalesced) whenever the screen may have changed.
func (t *Term) Updates() <-chan struct{} { return t.updates }

// Done is closed when the shell exits.
func (t *Term) Done() <-chan struct{} { return t.done }

// Exited reports whether the shell has exited.
func (t *Term) Exited() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Close hangs up the shell, as closing a terminal window does, and kills it
// if it is still running a moment later. It does not wait.
func (t *Term) Close() {
	if t.Exited() {
		return
	}
	hangup(t.pid)
	t.closePty()
	go func() {
		select {
		case <-t.done:
		case <-time.After(time.Second):
			_ = t.cmd.Process.Kill()
		}
	}()
}

// Send writes raw input to the shell.
func (t *Term) Send(p []byte) error {
	if t.Exited() {
		return ErrExited
	}
	_, err := t.pty.Write(p)
	return err
}

// SendKey forwards a key press, encoded for the program's current modes, and
// scrolls back to the live screen.
func (t *Term) SendKey(k tea.KeyMsg) error {
	t.mu.Lock()
	app, paste := t.screen.AppCursor(), t.screen.BracketedPaste()
	t.scroll = 0
	t.mu.Unlock()
	var b []byte
	if k.Paste {
		b = PasteBytes(k, paste)
	} else {
		b = KeyBytes(k, app)
	}
	if len(b) == 0 {
		return nil
	}
	return t.Send(b)
}

// Resize changes the pty and screen size; the shell gets SIGWINCH.
func (t *Term) Resize(cols, rows int) error {
	cols, rows = max(cols, 1), max(rows, 1)
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, r := t.screen.Size(); c == cols && r == rows {
		return nil
	}
	t.screen.Resize(cols, rows)
	if t.Exited() {
		return nil
	}
	return setSize(t.pty, cols, rows)
}

// Size returns the screen's columns and rows.
func (t *Term) Size() (cols, rows int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.screen.Size()
}

// AltScreen reports whether a full-screen program (an editor, a pager) is running.
func (t *Term) AltScreen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.screen.AltScreen()
}

// ScrollBy moves the view n lines back into history (negative: forward).
func (t *Term) ScrollBy(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.screen.AltScreen() {
		t.scroll = 0
		return
	}
	t.scroll = min(max(t.scroll+n, 0), len(t.screen.scrollback))
}

// Scrolled returns how many lines back into history the view is.
func (t *Term) Scrolled() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scroll
}

// Text returns the visible screen as plain text.
func (t *Term) Text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.screen.Text()
}

// View renders the screen with colors at the current scroll position, with
// the cursor drawn when showCursor is set.
func (t *Term) View(showCursor bool) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	cx, cy := -1, -1
	if showCursor && t.screen.CursorVisible() {
		cx, cy = t.screen.Cursor()
	}
	return t.screen.Render(t.scroll, cx, cy)
}
