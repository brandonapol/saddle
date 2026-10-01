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

func TestCompleteSlash(t *testing.T) {
	names := []string{"code-review", "compact", "security-review", "simplify", "understand-anything:understand", "understand-anything:understand-chat"}
	cases := []struct {
		in, want string
		n        int
	}{
		{"/sim", "/simplify ", 1},
		{"/co", "/co", 2},
		{"/com", "/compact ", 1},
		{"/understand-anything:", "/understand-anything:understand", 2},
		{"/understand-c", "/understand-anything:understand-chat ", 1}, // after the plugin prefix
		{"/nope", "/nope", 0},
		{"/sim args", "/sim args", 0},
		{"hello", "hello", 0},
	}
	for _, c := range cases {
		got, m := completeSlash(c.in, names)
		if got != c.want || len(m) != c.n {
			t.Errorf("completeSlash(%q) = %q, %v; want %q with %d matches", c.in, got, m, c.want, c.n)
		}
	}
}

func TestFindSkills(t *testing.T) {
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
