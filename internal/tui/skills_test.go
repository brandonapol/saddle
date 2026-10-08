package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseSlash(t *testing.T) {
	cases := []struct {
		in   string
		want slashCmd
		ok   bool
	}{
		{"/code-review", slashCmd{name: "code-review"}, true},
		{"  /simplify  ", slashCmd{name: "simplify"}, true},
		{"/code-review high --fix", slashCmd{name: "code-review", args: "high --fix"}, true},
		{"/understand-anything:understand src", slashCmd{name: "understand-anything:understand", args: "src"}, true},
		{"/loop\t5m /foo", slashCmd{name: "loop", args: "5m /foo"}, true},
		{"/", slashCmd{}, false},
		{"//comment", slashCmd{}, false},
		{"/tmp/x is full", slashCmd{}, false},
		{"/-flag", slashCmd{}, false},
		{"work #46 /simplify", slashCmd{}, false},
		{"", slashCmd{}, false},
	}
	for _, c := range cases {
		got, ok := parseSlash(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseSlash(%q) = %+v, %v; want %+v, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if s := (slashCmd{name: "code-review", args: "high"}).String(); s != "/code-review high" {
		t.Errorf("String() = %q", s)
	}
}

func TestSlashMatches(t *testing.T) {
	var es []slashEntry
	for _, n := range []string{"code-review", "compact", "security-review", "simplify", "understand-anything:understand", "understand-anything:understand-chat"} {
		es = append(es, slashEntry{name: n})
	}
	cases := []struct {
		prefix string
		want   []string
	}{
		{"sim", []string{"simplify"}},
		{"co", []string{"code-review", "compact"}},
		{"understand-anything:", []string{"understand-anything:understand", "understand-anything:understand-chat"}},
		{"understand-c", []string{"understand-anything:understand-chat"}}, // after the plugin prefix
		{"nope", nil},
	}
	for _, c := range cases {
		var got []string
		for _, e := range slashMatches(c.prefix, es) {
			got = append(got, e.name)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("slashMatches(%q) = %v, want %v", c.prefix, got, c.want)
		}
	}
}

func TestFindSkills(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, root := t.TempDir(), t.TempDir()
	plugin := filepath.Join(home, "plugin-install")
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude", "skills", "budget", "SKILL.md"), "")
	write(filepath.Join(home, ".claude", "commands", "git", "pr.md"), "")
	write(filepath.Join(root, ".claude", "skills", "deploy", "SKILL.md"), "")
	write(filepath.Join(plugin, "skills", "understand", "SKILL.md"), "")
	write(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"),
		`{"version":2,"plugins":{"ua@market":[{"installPath":"`+filepath.ToSlash(plugin)+`"}]}}`)

	got := findSkills(home, root)
	for _, want := range []string{"budget", "git:pr", "deploy", "ua:understand", "code-review"} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("findSkills missing %q in %v", want, got)
		}
	}
	if !reflect.DeepEqual(findSkills("", ""), builtinSkills) {
		t.Errorf("with no dirs, want only built-ins, got %v", findSkills("", ""))
	}
}

// CLAUDE_CONFIG_DIR moves Claude Code's user skills and plugins.
func TestFindSkillsHonorsClaudeConfigDir(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	if err := os.MkdirAll(filepath.Join(cfg, "skills", "moved"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "skills", "moved", "SKILL.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := findSkills(t.TempDir(), "")
	found := false
	for _, g := range got {
		found = found || g == "moved"
	}
	if !found {
		t.Errorf("findSkills ignored CLAUDE_CONFIG_DIR: %v", got)
	}
}
