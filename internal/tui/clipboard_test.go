package tui

import (
	"encoding/base64"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestOSC52Encoding(t *testing.T) {
	text := "line one\nline two ✓"
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
	if got := osc52(text); got != want {
		t.Errorf("osc52 = %q, want %q", got, want)
	}
	// tmux passes a DCS through only with every ESC inside doubled.
	wrapped := tmuxPassthrough(want)
	if !strings.HasPrefix(wrapped, "\x1bPtmux;\x1b\x1b]52;c;") || !strings.HasSuffix(wrapped, "\a\x1b\\") {
		t.Errorf("tmux wrapping = %q", wrapped)
	}
	if strings.Count(wrapped, "\x1b") != 4 {
		t.Errorf("want 4 ESCs (DCS, doubled OSC, ST), got %q", wrapped)
	}
}

// fakeClip is a clipboard whose environment, PATH and commands are made up.
type fakeClip struct {
	env   map[string]string
	path  []string // binaries that exist
	fail  map[string]error
	ran   []string // "name args…<<stdin"
	wrote strings.Builder
}

func (f *fakeClip) clipboard() clipboard {
	return clipboard{
		getenv: func(k string) string { return f.env[k] },
		lookPath: func(name string) (string, error) {
			for _, p := range f.path {
				if p == name {
					return "/usr/bin/" + name, nil
				}
			}
			return "", exec.ErrNotFound
		},
		run: func(name string, args []string, stdin string) error {
			f.ran = append(f.ran, strings.TrimSpace(name+" "+strings.Join(args, " "))+"<<"+stdin)
			return f.fail[name]
		},
		term: &f.wrote,
	}
}

func TestClipboardBackendSelection(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		path    []string
		fail    map[string]error
		wantRan []string
		wantVia string
		inFlash []string
	}{
		{name: "wayland", env: map[string]string{"WAYLAND_DISPLAY": "wayland-1", "DISPLAY": ":0"},
			path: []string{"wl-copy", "xclip"}, wantRan: []string{"wl-copy<<hi"}, wantVia: "OSC 52, wl-copy",
			inFlash: []string{"copied 2 lines", "wl-copy"}},
		{name: "x11 xclip", env: map[string]string{"DISPLAY": ":0"},
			path: []string{"wl-copy", "xclip", "xsel"}, wantRan: []string{"xclip -selection clipboard<<hi"}, wantVia: "OSC 52, xclip"},
		{name: "x11 xsel", env: map[string]string{"DISPLAY": ":0"},
			path: []string{"xsel"}, wantRan: []string{"xsel --clipboard --input<<hi"}, wantVia: "OSC 52, xsel"},
		{name: "mac", path: []string{"pbcopy"}, wantRan: []string{"pbcopy<<hi"}, wantVia: "OSC 52, pbcopy"},
		{name: "tmux and wayland", env: map[string]string{"TMUX": "/tmp/tmux-1/default,1,0", "WAYLAND_DISPLAY": "w"},
			path: []string{"wl-copy"}, wantRan: []string{"tmux load-buffer -w -<<hi", "wl-copy<<hi"}, wantVia: "OSC 52, tmux, wl-copy"},
		{name: "ssh: OSC 52 only", path: []string{"wl-copy"}, wantVia: "OSC 52",
			inFlash: []string{"OSC 52 only", "wl-copy"}},
		{name: "tool fails", env: map[string]string{"WAYLAND_DISPLAY": "w"}, path: []string{"wl-copy"},
			fail: map[string]error{"wl-copy": errors.New("exit status 1")}, wantRan: []string{"wl-copy<<hi"}, wantVia: "OSC 52",
			inFlash: []string{"wl-copy: exit status 1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeClip{env: c.env, path: c.path, fail: c.fail}
			r := f.clipboard().copy("hi")
			if got := strings.Join(f.ran, " | "); got != strings.Join(c.wantRan, " | ") {
				t.Errorf("ran %q, want %q", got, c.wantRan)
			}
			if got := strings.Join(r.via, ", "); got != c.wantVia {
				t.Errorf("via %q, want %q", got, c.wantVia)
			}
			wantSeq := osc52("hi")
			if c.env["TMUX"] != "" {
				wantSeq = tmuxPassthrough(wantSeq)
			}
			if f.wrote.String() != wantSeq {
				t.Errorf("terminal got %q, want %q", f.wrote.String(), wantSeq)
			}
			msg := r.flash(2)
			for _, s := range c.inFlash {
				if !strings.Contains(msg, s) {
					t.Errorf("flash %q lacks %q", msg, s)
				}
			}
		})
	}
}

// An old tmux without load-buffer -w still gets the text into its buffer.
func TestClipboardOldTmuxRetriesWithoutW(t *testing.T) {
	calls := 0
	cl := clipboard{
		getenv:   func(k string) string { return map[string]string{"TMUX": "x"}[k] },
		lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
		run: func(name string, args []string, _ string) error {
			calls++
			if strings.Join(args, " ") == "load-buffer -w -" {
				return errors.New("unknown flag -w")
			}
			return nil
		},
	}
	r := cl.copy("x")
	if calls != 2 || strings.Join(r.via, ",") != "tmux" || r.err != nil {
		t.Errorf("calls %d via %v err %v", calls, r.via, r.err)
	}
}

func TestClipboardFlashCountsLines(t *testing.T) {
	r := clipResult{via: []string{"OSC 52", "wl-copy"}, native: true}
	if got := r.flash(1); !strings.HasPrefix(got, "copied 1 line ") {
		t.Errorf("flash(1) = %q", got)
	}
	if got := (clipResult{err: errors.New("boom")}).flash(3); !strings.Contains(got, "copy failed") {
		t.Errorf("total failure flash = %q", got)
	}
}
