package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/trust"
)

// untrustedRepo is bareRepo with no trust decision and no SADDLE_TRUST,
// answering prompts from in as if on a terminal when tty.
func untrustedRepo(t *testing.T, tty bool) string {
	t.Helper()
	root := bareRepo(t)
	t.Setenv(trust.EnvTrust, "")
	old := stdinIsTTY
	stdinIsTTY = func(io.Reader) bool { return tty }
	t.Cleanup(func() { stdinIsTTY = old })
	t.Chdir(root)
	return root
}

func runIn(t *testing.T, in string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := Root()
	cmd.SetArgs(args)
	cmd.SetIn(strings.NewReader(in))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}

func noSaddleDir(t *testing.T, root string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".saddle")); !os.IsNotExist(err) {
		t.Fatalf(".saddle exists after an untrusted run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "hooks", "reference-transaction")); !os.IsNotExist(err) {
		t.Fatalf("hook installed after an untrusted run: %v", err)
	}
}

func trusted(t *testing.T, root string) bool {
	t.Helper()
	rep, err := trust.Status(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rep.State == trust.StateTrusted
}

func TestInitAsksBeforeWritingAndDeclineWritesNothing(t *testing.T) {
	root := untrustedRepo(t, true)
	old := stdoutIsTTY
	stdoutIsTTY = func(io.Writer) bool { return true }
	t.Cleanup(func() { stdoutIsTTY = old })
	t.Setenv("NO_COLOR", "1")
	out, err := runIn(t, "2\n", "init")
	if err == nil {
		t.Fatalf("declined init succeeded:\n%s", out)
	}
	banner := strings.Index(out, "Howdy")
	prompt := strings.Index(out, "Yes, trust this folder")
	if banner < 0 || prompt < 0 || banner > prompt {
		t.Fatalf("want the banner, then the prompt:\n%s", out)
	}
	noSaddleDir(t, root)
	if trusted(t, root) {
		t.Fatal("declining recorded trust")
	}

	out, err = runIn(t, "1\n", "init", "-q")
	if err != nil || !strings.Contains(out, "initialized") {
		t.Fatalf("trusted init: %v\n%s", err, out)
	}
	if !trusted(t, root) {
		t.Fatal("yes wasn't remembered")
	}
	// Remembered: init again asks nothing.
	out, err = runIn(t, "", "init", "-q")
	if err != nil || strings.Contains(out, "Do you trust") {
		t.Fatalf("remembered decision re-asked: %v\n%s", err, out)
	}
}

func TestInitNonInteractiveNeedsTrustFlag(t *testing.T) {
	root := untrustedRepo(t, false)
	out, err := runIn(t, "", "init", "-q")
	if err == nil || !strings.Contains(err.Error(), "--trust") {
		t.Fatalf("non-interactive init without --trust: %v\n%s", err, out)
	}
	noSaddleDir(t, root)

	if out, err := runIn(t, "", "init", "-q", "--trust"); err != nil {
		t.Fatalf("init --trust: %v\n%s", err, out)
	}
	if !trusted(t, root) {
		t.Fatal("--trust wasn't remembered")
	}
}

func TestInitSaddleTrustEnv(t *testing.T) {
	root := untrustedRepo(t, false)
	t.Setenv(trust.EnvTrust, "1")
	if out, err := runIn(t, "", "init", "-q"); err != nil {
		t.Fatalf("SADDLE_TRUST=1 init: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, ".saddle", "config.toml")); err != nil {
		t.Fatal(err)
	}
}

func TestUntrustedUpRefuses(t *testing.T) {
	root := untrustedRepo(t, false)
	out, err := runIn(t, "", "up", "--skip-doctor")
	if err == nil || !strings.Contains(err.Error(), "saddle trust") {
		t.Fatalf("untrusted up: %v\n%s", err, out)
	}
	noSaddleDir(t, root)
}

func TestUpAsksOnceInAnInitializedRepo(t *testing.T) {
	// A repo initialized before trust existed: the first up asks.
	root := untrustedRepo(t, true)
	t.Setenv(trust.EnvTrust, "1")
	if out, err := runIn(t, "", "init", "-q"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	t.Setenv(trust.EnvTrust, "")
	out, err := runIn(t, "no\n", "up", "--skip-doctor")
	if err == nil || !strings.Contains(out, "Do you trust") || !strings.Contains(err.Error(), "saddle trust") {
		t.Fatalf("declined up: %v\n%s", err, out)
	}
	if trusted(t, root) {
		t.Fatal("declined up recorded trust")
	}
}

func TestWorktreeAgentsAreNotAsked(t *testing.T) {
	untrustedRepo(t, false)
	t.Setenv("SADDLE_TASK", "t3")
	if err := gateTrust(&bytes.Buffer{}, strings.NewReader(""), false); err != nil {
		t.Fatalf("agent blocked by the trust prompt: %v", err)
	}
}

func TestTrustCommands(t *testing.T) {
	root := untrustedRepo(t, false)
	out, err := runIn(t, "", "trust", "status")
	if err != nil || !strings.Contains(out, "not trusted") || !strings.Contains(out, root) {
		t.Fatalf("status before: %v\n%s", err, out)
	}
	// Non-interactive without --yes shows the prompt and records nothing.
	out, err = runIn(t, "", "trust")
	if err == nil || !strings.Contains(out, ".saddle/") || trusted(t, root) {
		t.Fatalf("trust without a TTY or --yes: %v\n%s", err, out)
	}
	if out, err := runIn(t, "", "trust", "--yes"); err != nil || !trusted(t, root) {
		t.Fatalf("trust --yes: %v\n%s", err, out)
	}
	out, _ = runIn(t, "", "trust", "status")
	if !strings.Contains(out, "trusted") || strings.Contains(out, "not trusted") {
		t.Fatalf("status after trust:\n%s", out)
	}
	gitRun(t, root, "remote", "add", "origin", "https://example.com/other.git")
	out, _ = runIn(t, "", "trust", "status")
	if !strings.Contains(out, "origin changed") {
		t.Fatalf("status after the origin changed:\n%s", out)
	}
	if _, err := runIn(t, "", "trust", "--yes"); err != nil {
		t.Fatal(err)
	}
	if out, err := runIn(t, "", "untrust"); err != nil || trusted(t, root) {
		t.Fatalf("untrust: %v\n%s", err, out)
	}
	// Interactive saddle trust asks.
	stdinIsTTY = func(io.Reader) bool { return true }
	if out, err := runIn(t, "1\n", "trust"); err != nil || !strings.Contains(out, "Do you trust") || !trusted(t, root) {
		t.Fatalf("interactive trust: %v\n%s", err, out)
	}
}

func TestPluginOnboardAsksTrustInChatFirst(t *testing.T) {
	root := untrustedRepo(t, false)
	doc := &fakeDoctor{}
	var out bytes.Buffer
	if onboard(&out, root, doc.run) {
		t.Fatalf("untrusted first use went on:\n%s", out.String())
	}
	got := out.String()
	for _, want := range []string{"Howdy", "Do you trust", ".claude/settings.local.json", "1. Yes, trust this folder", "2. No, exit", "Ask the user", "saddle trust --yes"} {
		if !strings.Contains(got, want) {
			t.Errorf("plugin trust prompt lacks %q:\n%s", want, got)
		}
	}
	if banner, prompt := strings.Index(got, "Howdy"), strings.Index(got, "Do you trust"); banner > prompt {
		t.Errorf("banner after the prompt:\n%s", got)
	}
	if doc.runs != 0 {
		t.Fatal("doctor ran before trust")
	}
	noSaddleDir(t, root)
	if trusted(t, root) {
		t.Fatal("onboarding recorded trust without the user's answer")
	}

	// The user said yes in chat; the session ran saddle trust --yes.
	if out, err := runIn(t, "", "trust", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out.Reset()
	if !onboard(&out, root, doc.run) || doc.runs != 1 {
		t.Fatalf("trusted first use: runs %d\n%s", doc.runs, out.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".saddle", "config.toml")); err != nil {
		t.Fatal(err)
	}
}

func TestPluginOnboardAsksInAnInitializedUntrustedRepo(t *testing.T) {
	root := untrustedRepo(t, false)
	t.Setenv(trust.EnvTrust, "1")
	if out, err := runIn(t, "", "init", "-q"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Setenv(trust.EnvTrust, "")
	var out bytes.Buffer
	if onboard(&out, root, (&fakeDoctor{}).run) || !strings.Contains(out.String(), "Do you trust") || strings.Contains(out.String(), "Howdy") {
		t.Fatalf("initialized, untrusted:\n%s", out.String())
	}
}

func TestPluginEngineRefusesUntrustedRepo(t *testing.T) {
	untrustedRepo(t, false)
	t.Setenv(trust.EnvTrust, "1")
	if out, err := runIn(t, "", "init", "-q"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Setenv(trust.EnvTrust, "")
	if out, err := runIn(t, "", "plugin", "engine"); err == nil || !strings.Contains(err.Error(), "saddle trust") {
		t.Fatalf("engine in an untrusted repo: %v\n%s", err, out)
	}
}
