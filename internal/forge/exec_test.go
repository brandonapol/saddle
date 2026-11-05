package forge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// fakePATH puts a gh script on a PATH of its own: it echoes its args, and
// fails with a message on stderr when the first arg is "fail".
func fakePATH(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = fail ]; then echo "half out"; echo "it broke" >&2; exit 3; fi
echo "  $*  "
`
	if err := os.WriteFile(filepath.Join(bin, GH), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return bin
}

func TestCLIRunsGHOnPATH(t *testing.T) {
	fakePATH(t)
	dir := t.TempDir()
	out, err := CLI(dir, GH)(context.Background(), "pr", "view", "7")
	if err != nil || out != "pr view 7" {
		t.Fatalf("CLI = %q, %v; want trimmed stdout", out, err)
	}
	out, err = CLI(dir, GH)(context.Background(), "fail", "now", "please")
	if err == nil || err.Error() != "gh fail now: exit status 3: it broke" {
		t.Fatalf("CLI error = %v, want the CLI, two args and stderr", err)
	}
	if out != "half out" {
		t.Fatalf("CLI stdout on failure = %q, want what gh printed (pr checks prints JSON and exits non-zero)", out)
	}
}

func TestOutputKeepsStreamsApart(t *testing.T) {
	fakePATH(t)
	out, errb, err := Output(context.Background(), t.TempDir(), GH, "fail")
	if err == nil || out != "half out\n" || errb != "it broke\n" {
		t.Fatalf("Output = %q, %q, %v", out, errb, err)
	}
	comb, err := CombinedOutput(context.Background(), t.TempDir(), GH, "fail")
	if err == nil || !strings.Contains(comb, "half out") || !strings.Contains(comb, "it broke") {
		t.Fatalf("CombinedOutput = %q, %v", comb, err)
	}
}

// TestNewGitHubUsesGHOnPATH drives the selected forge end to end through a
// fake gh, as the e2e harness does with internal/e2e/fakegh.
func TestNewGitHubUsesGHOnPATH(t *testing.T) {
	fakePATH(t)
	url, err := New(config.Forge{}, t.TempDir()).CreatePR(context.Background(), NewPR{Base: "main", Head: "b", Title: "t", Body: "x"})
	if err != nil || url != "pr create --base main --head b --title t --body x" {
		t.Fatalf("CreatePR via fake gh = %q, %v", url, err)
	}
}

func TestIsCLI(t *testing.T) {
	if !IsCLI("gh") || !IsCLI("glab") || IsCLI("git") {
		t.Fatal("IsCLI: want gh and glab only")
	}
}

// #788: a gh call on a dead connection hung a watcher. Every call now has a
// deadline and says it timed out, a wait rather than a failure.
func TestOutputTimesOut(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, GH), []byte("#!/bin/sh\nsleep 3600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := CallTimeout
	CallTimeout = 300 * time.Millisecond
	t.Cleanup(func() { CallTimeout = old })
	start := time.Now()
	_, err := CLI(t.TempDir(), GH)(context.Background(), "pr", "checks", "7")
	if !errors.Is(err, ErrNetTimeout) || !Transient(err) {
		t.Fatalf("stalled gh = %v, want ErrNetTimeout", err)
	}
	if errors.Is(err, ErrRateLimited) {
		t.Fatal("a timeout reads as the rate limit")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("stalled gh took %s over a 300ms deadline", took)
	}
}

func TestCallContextLogDownloadGetsLonger(t *testing.T) {
	_, d, cancel := callContext(context.Background(), []string{"run", "view", "--job", "1", "--log-failed"})
	cancel()
	if d != LogTimeout {
		t.Fatalf("run view --log-failed deadline = %s, want LogTimeout %s", d, LogTimeout)
	}
	_, d, cancel = callContext(context.Background(), []string{"pr", "view", "--log"})
	cancel()
	if d != CallTimeout {
		t.Fatalf("pr view deadline = %s, want CallTimeout %s", d, CallTimeout)
	}
}
