package hook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/runq"
)

// The PreToolUse rewrite (#240): heavy commands go through saddle run, the
// rest stay as they were. A line is wrapped whole only when everything in it
// is a heavy tool, one of the repo's tools, or a read-only helper, because
// the rewrite is pre-approved (Claude Code drops an updatedInput that
// doesn't come with allow or ask).
func TestHeavyRewrite(t *testing.T) {
	const run = "/b/saddle run --class "
	m := runq.NewMatcher(runq.Config{})
	for cmd, want := range map[string]string{
		"make check":                       run + "go-test --prio worker -- make check",
		"go test ./... -race":              run + "go-test --prio worker -- go test ./... -race",
		"go test -run 'TestA|TestB' ./x":   run + "go-test --prio worker -- go test -run 'TestA|TestB' ./x",
		"golangci-lint run ./...":          run + "golangci-lint --prio worker -- golangci-lint run ./...",
		"flutter test test/widget_test.go": run + "flutter-test --prio worker -- flutter test test/widget_test.go",
		"go test ./... >/dev/null 2>&1":    run + "go-test --prio worker -- go test ./... >/dev/null 2>&1",
		"make check;":                      run + "go-test --prio worker -- make check;",
		"go test ./... # all of it":        run + "go-test --prio worker -- go test ./... # all of it",
		// Compound lines are wrapped whole: one lease for the line.
		"go test ./... 2>&1 | tail -50":   run + "go-test --prio worker -- bash -c 'go test ./... 2>&1 | tail -50'",
		"cd internal && go test ./...":    run + "go-test --prio worker -- bash -c 'cd internal && go test ./...'",
		"CGO_ENABLED=0 go test ./...":     run + "go-test --prio worker -- bash -c 'CGO_ENABLED=0 go test ./...'",
		"time make check":                 run + "go-test --prio worker -- bash -c 'time make check'",
		"go build ./... && go test ./...": run + "go-test --prio worker -- bash -c 'go build ./... && go test ./...'",
		"make fix && make check":          run + "go-test --prio worker -- bash -c 'make fix && make check'",
		"go vet ./...\ngo test ./...":     run + "go-test --prio worker -- bash -c 'go vet ./...\ngo test ./...'",
		"go test -run 'A' ./x | cat":      run + "go-test --prio worker -- bash -c 'go test -run '\\''A'\\'' ./x | cat'",
		// Light commands pass.
		"go version":                       "",
		"go build ./...":                   "",
		"ls -la":                           "",
		"git commit -m 'make check'":       "",
		"echo make check":                  "",
		"flutter doctor && dart --version": "",
		// Already wrapped, or bypassed.
		"saddle run --class go-test -- go test ./...":  "",
		"/b/saddle run --class go-test -- make check":  "",
		"cd x && saddle run --class e2e -- make check": "",
		"SADDLE_RUNQ=off make check":                   "",
		"SADDLE_RUNQ=off go test ./... | tail":         "",
		// What the rewrite can't vouch for is left to the shims.
		"make check && git push":      "",
		"go test ./... > out.txt":     "",
		"go test ./... | tee log":     "",
		"go test $(go list ./...)":    "",
		"go test `go list ./...`":     "",
		"go test \"$(cat pkgs)\"":     "",
		"go test ./... &":             "",
		"(cd x && go test ./...)":     "",
		"go test ./... < input":       "",
		"cat <<EOF | go test\nx\nEOF": "",
		"go test ./... |& tail":       "",
		"sh -c 'go test ./...'":       "",
	} {
		got, _ := HeavyRewrite(cmd, m, "/b/saddle")
		if got != want {
			t.Errorf("HeavyRewrite(%q)\n got: %s\nwant: %s", cmd, got, want)
		}
	}
}

// The binary path is quoted when it needs to be.
func TestHeavyRewriteQuotesBinary(t *testing.T) {
	got, class := HeavyRewrite("make check", runq.NewMatcher(runq.Config{}), "/my dir/saddle")
	if want := "'/my dir/saddle' run --class go-test --prio worker -- make check"; got != want || class != "go-test" {
		t.Fatalf("got %q (%s), want %q", got, class, want)
	}
}

func preToolUseBash(cmd string) map[string]any {
	return map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Bash", "permission_mode": "auto",
		"tool_input": map[string]any{"command": cmd, "description": "run the checks", "timeout": 600000.0},
	}
}

func TestPreToolUseRewritesHeavyBash(t *testing.T) {
	a := setup(t)
	t1, _ := a.Spawn(app.SpawnReq{Title: "one"})
	executable = func() (string, error) { return "/b/saddle", nil }
	t.Cleanup(func() { executable = os.Executable })
	t.Setenv(runq.EnvBypass, "")
	t.Setenv(runq.EnvLease, "")

	out := run(t, a, t1.ID, preToolUseBash("make check"))
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	in, _ := hs["updatedInput"].(map[string]any)
	if hs["permissionDecision"] != "allow" || in["command"] != "/b/saddle run --class go-test --prio worker -- make check" {
		t.Fatalf("out = %v", out)
	}
	// The rest of the tool input rides along: Claude replaces it whole.
	if in["description"] != "run the checks" || in["timeout"] != 600000.0 {
		t.Fatalf("updatedInput dropped fields: %v", in)
	}
	if r, _ := hs["permissionDecisionReason"].(string); r == "" {
		t.Fatal("no reason")
	}

	for name, tc := range map[string]struct {
		env     map[string]string
		in      map[string]any
		runqTOM string
	}{
		"light":              {in: preToolUseBash("go build ./...")},
		"already wrapped":    {in: preToolUseBash("saddle run --class go-test --prio worker -- make check")},
		"SADDLE_RUNQ=off":    {env: map[string]string{runq.EnvBypass: "off"}, in: preToolUseBash("make check")},
		"inside a lease":     {env: map[string]string{runq.EnvLease: "tok"}, in: preToolUseBash("make check")},
		"mode off in config": {runqTOM: "mode = \"off\"\n", in: preToolUseBash("make check")},
		"no patterns":        {runqTOM: "[classes.go-test]\nmatch = []\n", in: preToolUseBash("make check")},
		"plan mode": {in: func() map[string]any {
			m := preToolUseBash("make check")
			m["permission_mode"] = "plan"
			return m
		}()},
		"grok": {in: map[string]any{"hookEventName": "PreToolUse", "toolName": "Bash",
			"toolInput": map[string]any{"command": "make check"}}},
		"not bash": {in: map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read",
			"tool_input": map[string]any{"command": "make check"}}},
	} {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			p := app.RunqConfigPath(a.Root)
			if tc.runqTOM != "" {
				if err := os.WriteFile(p, []byte(tc.runqTOM), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(p) })
			}
			if out := run(t, a, t1.ID, tc.in); out != nil {
				t.Fatalf("rewrote: %v", out)
			}
		})
	}
}

// A repo's runq.toml adds its own heavy commands.
func TestPreToolUseRewriteUsesRepoPatterns(t *testing.T) {
	a := setup(t)
	t1, _ := a.Spawn(app.SpawnReq{Title: "one"})
	executable = func() (string, error) { return "/b/saddle", nil }
	t.Cleanup(func() { executable = os.Executable })
	t.Setenv(runq.EnvBypass, "")
	t.Setenv(runq.EnvLease, "")
	if err := os.WriteFile(filepath.Join(a.Root, ".saddle", "runq.toml"), []byte("[classes.e2e]\nmatch = [\"make e2e*\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := run(t, a, t1.ID, preToolUseBash("make e2e-fast"))
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	in, _ := hs["updatedInput"].(map[string]any)
	if in["command"] != "/b/saddle run --class e2e --prio worker -- make e2e-fast" {
		t.Fatalf("out = %v", out)
	}
}

// --no-verify is still denied when the line is also heavy.
func TestPreToolUseNoVerifyBeatsRewrite(t *testing.T) {
	a := setup(t)
	t1, _ := a.Spawn(app.SpawnReq{Title: "one"})
	out := run(t, a, t1.ID, preToolUseBash("make check && git commit -n -m x"))
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	if hs["permissionDecision"] != "deny" {
		t.Fatalf("out = %v", out)
	}
}

// The parser behind the rewrite and the --no-verify check.
func TestParseShell(t *testing.T) {
	cmds, ok := parseShell("FOO=1 go test ./... 2>&1 | tail -n 3 >/dev/null; cd 'a b' && echo \"x;y\" # c")
	if !ok {
		t.Fatal("not ok")
	}
	want := []struct {
		words []string
		sep   string
	}{
		{[]string{"FOO=1", "go", "test", "./..."}, "|"},
		{[]string{"tail", "-n", "3"}, ";"},
		{[]string{"cd", "a b"}, "&&"},
		{[]string{"echo", "x;y"}, ""},
	}
	if len(cmds) != len(want) {
		t.Fatalf("cmds = %+v", cmds)
	}
	for i, w := range want {
		if !equal(cmds[i].words, w.words) || cmds[i].sep != w.sep {
			t.Errorf("cmd %d = %+v, want %+v", i, cmds[i], w)
		}
	}
	if r := cmds[0].redirs; len(r) != 1 || r[0].op != "2>&" || r[0].target != "1" {
		t.Errorf("redirs = %+v", r)
	}
	if r := cmds[1].redirs; len(r) != 1 || r[0].op != ">" || r[0].target != "/dev/null" {
		t.Errorf("redirs = %+v", r)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
