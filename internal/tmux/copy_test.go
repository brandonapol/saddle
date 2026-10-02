package tmux

import (
	"strings"
	"testing"
)

// record swaps run for a recorder and returns the joined commands it saw.
func record(t *testing.T) *[]string {
	var got []string
	old := run
	run = func(args ...string) (string, error) {
		got = append(got, strings.Join(args, " "))
		return "", nil
	}
	t.Cleanup(func() { run = old })
	return &got
}

func TestConfigureSessionScopedMouse(t *testing.T) {
	got := record(t)
	if err := (Tmux{Session: "saddle-x"}).configureSession("wl-copy"); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(*got, "\n")
	if !strings.Contains(all, "set-option -t =saddle-x mouse on") {
		t.Fatalf("mouse not enabled on the session only:\n%s", all)
	}
	for _, c := range *got {
		if strings.HasPrefix(c, "set-option") && !strings.Contains(c, "-t =saddle-x") {
			t.Fatalf("option not scoped to session: %s", c)
		}
		if strings.Contains(c, " -g ") {
			t.Fatalf("global option touched: %s", c)
		}
	}
}

func TestConfigureSessionCopyBindingWlCopy(t *testing.T) {
	got := record(t)
	_ = Tmux{Session: "saddle-x"}.configureSession("wl-copy")
	for _, table := range []string{"copy-mode", "copy-mode-vi"} {
		found := false
		for _, c := range *got {
			if strings.HasPrefix(c, "bind-key -T "+table+" MouseDragEnd1Pane") &&
				strings.Contains(c, "#{==:#{session_name},saddle-x}") &&
				strings.Contains(c, "copy-pipe-and-cancel wl-copy") &&
				strings.Contains(c, "copy-pipe-and-cancel") {
				found = true
			}
		}
		if !found {
			t.Errorf("no session-guarded wl-copy binding in %s: %v", table, *got)
		}
	}
}

func TestConfigureSessionFallbackOSC52(t *testing.T) {
	got := record(t)
	_ = Tmux{Session: "saddle-x"}.configureSession("")
	all := strings.Join(*got, "\n")
	if strings.Contains(all, "wl-copy") {
		t.Fatalf("wl-copy used though missing:\n%s", all)
	}
	if !strings.Contains(all, "set-option -s set-clipboard on") {
		t.Fatalf("set-clipboard not enabled for OSC52 fallback:\n%s", all)
	}
	if !strings.Contains(all, "MouseDragEnd1Pane") {
		t.Fatalf("drag-end binding missing:\n%s", all)
	}
}

func TestNewSessionConfiguresSession(t *testing.T) {
	got := record(t)
	_, _ = Tmux{Session: "saddle-x"}.NewSession("w", "/tmp", "sh")
	if len(*got) < 2 || !strings.HasPrefix((*got)[0], "new-session") ||
		!strings.Contains(strings.Join(*got, "\n"), "mouse on") {
		t.Fatalf("NewSession did not configure: %v", *got)
	}
}
