package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// These tests check launch lines against the coding CLIs installed on this
// machine and skip when one is missing.

// TestGrokLaunchRunsUnderInstalledGrok runs a worker's launch.sh with the real
// grok, --help appended before the prompt: grok parses every flag before
// printing help, so an unknown flag (#181: --directory, --prompt) exits 2.
func TestGrokLaunchRunsUnderInstalledGrok(t *testing.T) {
	real, err := exec.LookPath("grok")
	if err != nil {
		t.Skip("grok not installed")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	tmp := t.TempDir()
	status := filepath.Join(tmp, "status")
	wrap := filepath.Join(tmp, "grok-check")
	body := "#!/usr/bin/env bash\nargs=()\nfor a in \"$@\"; do [ \"$a\" = -- ] && break; args+=(\"$a\"); done\n" +
		shellQuote(real) + " \"${args[@]}\" --help >/dev/null 2>" + shellQuote(filepath.Join(tmp, "err")) + "\necho $? >" + shellQuote(status) + "\n"
	if err := os.WriteFile(wrap, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	l := Launch{Root: dir, Bin: "true", Task: "t6", Title: "x", Dir: dir, Cmd: wrap, Model: "grok-4",
		Brief: "brief", Prompt: "go", RunDir: t.TempDir(), Deny: []string{"Agent", "Bash(git push:*)"}}
	cmd, err := Grok{}.Launch(l)
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("bash", "-c", cmd)
	c.Env = append(os.Environ(), "SHELL=true")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("launch.sh: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(status)
	if strings.TrimSpace(string(got)) != "0" {
		e, _ := os.ReadFile(filepath.Join(tmp, "err"))
		t.Fatalf("installed grok rejected the launch flags (exit %s): %s", strings.TrimSpace(string(got)), e)
	}
}

// helpFlags returns the long and short flags a CLI's --help lists.
func helpFlags(t *testing.T, cli string) map[string]bool {
	t.Helper()
	path, err := exec.LookPath(cli)
	if err != nil {
		t.Skip(cli + " not installed")
	}
	out, _ := exec.Command(path, "--help").CombinedOutput()
	flags := map[string]bool{}
	for _, f := range regexp.MustCompile(`(?:^|[\s,])(--?[A-Za-z][\w-]*)`).FindAllStringSubmatch(string(out), -1) {
		flags[f[1]] = true
	}
	return flags
}

// launchFlags lists the flags on the agent command line of a launch.sh.
func launchFlags(script, cli string) []string {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		if !strings.HasPrefix(line, shellQuote(cli)) {
			continue
		}
		for _, f := range regexp.MustCompile(`(?:^| )'?(--?[A-Za-z][\w-]*)'?`).FindAllStringSubmatch(line, -1) {
			out = append(out, f[1])
		}
	}
	return out
}

// Gemini and Codex accept unknown flags next to --help, so their launch
// flags are checked against the flags --help lists.
func TestHooklessLaunchFlagsAreInInstalledHelp(t *testing.T) {
	for _, name := range []string{"codex", Gemini{}.Name()} {
		t.Run(name, func(t *testing.T) {
			known := helpFlags(t, name)
			a, err := ByName(name)
			if err != nil {
				t.Fatal(err)
			}
			l := Launch{Root: "/repo", Bin: "/bin/saddle", Task: "t5", Title: "x", Dir: "/repo/wt", Model: "m",
				Mode: "bypassPermissions", Brief: "b", Prompt: "p", RunDir: t.TempDir()}
			_, script, _ := launchScript(t, a, l)
			flags := launchFlags(script, name)
			if len(flags) == 0 {
				t.Fatalf("no %s command line in:\n%s", name, script)
			}
			for _, f := range flags {
				if !known[f] {
					t.Errorf("%s --help does not list %s:\n%s", name, f, script)
				}
			}
		})
	}
}
