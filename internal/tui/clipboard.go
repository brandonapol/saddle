package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// clipboard puts text on the system clipboard every way that might reach
// it: an OSC 52 escape to the terminal (works over ssh, and through tmux with
// allow-passthrough), tmux's paste buffer when inside tmux (load-buffer -w
// also forwards it to the outer terminal), and the first native tool for the
// display server. Fields are swapped out in tests.
type clipboard struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	run      func(name string, args []string, stdin string) error
	term     io.Writer // where OSC 52 goes; nil skips it
}

func systemClipboard() clipboard {
	return clipboard{getenv: os.Getenv, lookPath: exec.LookPath, run: runStdin, term: os.Stdout}
}

// runStdin runs name with stdin. Its output goes nowhere: wl-copy and xclip
// fork a child that keeps serving the selection, and a pipe it inherited
// would keep Run waiting on it.
func runStdin(name string, args []string, stdin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.Run()
}

// clipTool is a native clipboard command and the display it needs.
type clipTool struct {
	name    string
	args    []string
	display string // env var that must be set; empty for none
}

var clipTools = []clipTool{
	{"wl-copy", nil, "WAYLAND_DISPLAY"},
	{"xclip", []string{"-selection", "clipboard"}, "DISPLAY"},
	{"xsel", []string{"--clipboard", "--input"}, "DISPLAY"},
	{"pbcopy", nil, ""},
}

// clipResult says which backends took the text.
type clipResult struct {
	via    []string
	native bool // a native tool or tmux was tried, not just OSC 52
	err    error
}

// osc52 is the escape that asks the terminal to set its clipboard.
func osc52(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
}

// tmuxPassthrough wraps seq so tmux hands it to the outer terminal.
func tmuxPassthrough(seq string) string {
	return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
}

func (c clipboard) copy(text string) clipResult {
	var r clipResult
	var errs []error
	inTmux := c.getenv("TMUX") != ""
	if c.term != nil {
		seq := osc52(text)
		if inTmux {
			seq = tmuxPassthrough(seq)
		}
		if _, err := io.WriteString(c.term, seq); err == nil {
			r.via = append(r.via, "OSC 52")
		}
	}
	if inTmux {
		r.native = true
		err := c.run("tmux", []string{"load-buffer", "-w", "-"}, text)
		if err != nil {
			// tmux before 3.2 has no -w; the buffer alone still helps.
			err = c.run("tmux", []string{"load-buffer", "-"}, text)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("tmux: %w", err))
		} else {
			r.via = append(r.via, "tmux")
		}
	}
	for _, t := range clipTools {
		if t.display != "" && c.getenv(t.display) == "" {
			continue
		}
		if _, err := c.lookPath(t.name); err != nil {
			continue
		}
		r.native = true
		if err := c.run(t.name, t.args, text); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.name, err))
		} else {
			r.via = append(r.via, t.name)
		}
		break
	}
	r.err = errors.Join(errs...)
	return r
}

// flash is the footer message for copying n lines.
func (r clipResult) flash(n int) string {
	what := fmt.Sprintf("%d lines", n)
	if n == 1 {
		what = "1 line"
	}
	if len(r.via) == 0 {
		if r.err != nil {
			return "copy failed: " + r.err.Error()
		}
		return "copy failed: no clipboard (install wl-copy, xclip or xsel)"
	}
	msg := "copied " + what + " (" + strings.Join(r.via, ", ") + ")"
	if !r.native {
		msg = "copied " + what + " via OSC 52 only: needs a terminal that allows it; or install wl-copy, xclip or xsel"
	}
	if r.err != nil {
		msg += "; " + r.err.Error()
	}
	return msg
}
