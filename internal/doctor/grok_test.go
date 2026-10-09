package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
)

func grokEnv(t *testing.T) *fakeEnv {
	f := healthy(t)
	f.cfg.Harness = config.HarnessGrok
	f.cfg.Grok.Cmd = "grok"
	f.paths["grok"] = true
	f.exec["grok --version"] = res{out: "grok 0.9.3\nextra"}
	return f
}

func TestGrokCheck(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeEnv)
		status Status
		detail string
	}{
		{"ok", func(*fakeEnv) {}, OK, "grok 0.9.3"},
		{"missing", func(f *fakeEnv) { f.paths["grok"] = false }, Fail, "not on PATH"},
		{"not runnable", func(f *fakeEnv) { f.exec["grok --version"] = res{err: errors.New("exit status 127")} }, Fail, "does not run"},
		{"no version", func(f *fakeEnv) { f.exec["grok --version"] = res{} }, Warn, "prints no version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := grokEnv(t)
			c.mutate(f)
			r := find(t, Run(f), CheckGrok)
			if r.Status != c.status || !strings.Contains(r.Detail, c.detail) {
				t.Fatalf("%s %q, want %s containing %q", r.Status, r.Detail, c.status, c.detail)
			}
			if r.About == "" {
				t.Error("no plain-words explanation")
			}
			if c.status != OK && r.Fix == "" {
				t.Error("no fix")
			}
		})
	}
}

// Under the default harness the check is skipped: healthy repos report the same checks.
func TestGrokCheckSkippedForClaudeHarness(t *testing.T) {
	for _, r := range Run(healthy(t)) {
		if r.Name == CheckGrok {
			t.Fatal("grok check ran under the claude harness")
		}
	}
}
