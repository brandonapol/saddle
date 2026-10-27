package fakeopencode

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// The fake answers run --format json like opencode: a session per new run,
// -s resumes it, an unknown -s fails without JSON, and QUOTA fails the turn.
func TestRunTurns(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	run := func(stdin string, args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"run", "--format", "json"}, args...), strings.NewReader(stdin), &out, &errb)
		return code, out.String(), errb.String()
	}
	code, out, errs := run("hello\nTOKENS 150000\n", "-m", "opencode/big-pickle", "--auto")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	for _, want := range []string{`"type":"step_start"`, `"sessionID":"ses_fake0001"`, `fakeopencode heard: hello`, `"read":149990`, `"type":"step_finish"`} {
		if !strings.Contains(out, want) {
			t.Errorf("events lack %s:\n%s", want, out)
		}
	}
	if code, out, _ := run("again", "-s", "ses_fake0001"); code != 0 || !strings.Contains(out, `"sessionID":"ses_fake0001"`) {
		t.Errorf("resume: exit %d, %s", code, out)
	}
	if code, out, errs := run("x", "-s", "ses_gone"); code != 1 || out != "" || !strings.Contains(errs, "Session not found") {
		t.Errorf("unknown session: exit %d, out %q, err %q", code, out, errs)
	}
	if code, out, _ := run("QUOTA"); code != 1 || !strings.Contains(out, `"type":"error"`) || !strings.Contains(out, "Insufficient balance") {
		t.Errorf("quota: exit %d, %s", code, out)
	}
	if log, _ := os.ReadFile(LogPath()); !strings.Contains(string(log), "--- turn ses_fake0001\nagain") {
		t.Errorf("log lacks the resumed turn: %q", log)
	}
}
