package tmux

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Several panes are captured in one tmux process (#272).
func TestCaptureManyIsOneCall(t *testing.T) {
	got := record(t)
	_, _ = Tmux{Session: "s"}.CaptureMany(map[string]int{"@1": 200, "@2": 25, "@3": 25})
	if len(*got) != 1 {
		t.Fatalf("tmux calls = %d, want 1:\n%s", len(*got), strings.Join(*got, "\n"))
	}
	for _, want := range []string{"capture-pane -p -t @1 -S -200", "capture-pane -p -t @2 -S -25", "capture-pane -p -t @3 -S -25"} {
		if !strings.Contains((*got)[0], want) {
			t.Errorf("call lacks %q: %s", want, (*got)[0])
		}
	}
	if n, err := (Tmux{}).CaptureMany(nil); err != nil || len(n) != 0 || len(*got) != 1 {
		t.Fatalf("an empty batch ran tmux: %v %v", n, err)
	}
}

// With a real tmux server, CaptureMany splits each pane's text out right
// and Windows lists the live windows.
func TestCaptureManyRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir, err := os.MkdirTemp("", "tmuxb")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	t.Setenv("TMUX", "")
	x := Tmux{Session: "batch-test"}
	t.Cleanup(func() { _ = x.KillSession(); _, _ = run("kill-server"); _ = os.RemoveAll(dir) })
	w1, err := x.NewSession("a", dir, "printf 'one\\nalpha\\n'; exec cat")
	if err != nil {
		t.Fatal(err)
	}
	w2, err := x.NewWindow("b", dir, "printf 'two\\nbeta\\n'; exec cat")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		out, err = x.CaptureMany(map[string]int{w1: 50, w2: 50})
		if err == nil && strings.Contains(out[w1], "alpha") && strings.Contains(out[w2], "beta") {
			break
		}
	}
	if err != nil || !strings.Contains(out[w1], "one\nalpha") || strings.Contains(out[w1], "beta") ||
		!strings.Contains(out[w2], "two\nbeta") || strings.Contains(out[w2], "alpha") {
		t.Fatalf("CaptureMany = %q, %v", out, err)
	}
	ws, err := x.Windows()
	if err != nil || !ws[w1] || !ws[w2] || ws["@999"] {
		t.Fatalf("Windows = %v, %v", ws, err)
	}
}
