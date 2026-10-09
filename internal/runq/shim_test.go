package runq

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMatcherClasses(t *testing.T) {
	m := NewMatcher(Config{})
	for argv, want := range map[string]string{
		"go test ./...": "go-test",
		"go test":       "go-test",
		"/usr/local/go/bin/go test -race ./internal/...": "go-test",
		"go version":              "",
		"go build ./...":          "",
		"go":                      "",
		"make check":              "go-test",
		"make check -j4":          "", // "make check" is exact
		"make build":              "",
		"make test":               "go-test",
		"make test-flutter":       "flutter-test", // the longer pattern wins
		"golangci-lint run ./...": "golangci-lint",
		"golangci-lint version":   "",
		"flutter test":            "flutter-test",
		"flutter doctor":          "",
		"dart --version":          "",
		"flutter analyze lib":     "generic-heavy",
		"gotest ./...":            "",
	} {
		if got := m.Class(strings.Fields(argv)); got != want {
			t.Errorf("Class(%q) = %q, want %q", argv, got, want)
		}
	}
}

func TestMatcherConfigOverridesDefaults(t *testing.T) {
	m := NewMatcher(Config{Classes: map[string]ClassConfig{
		"go-test": {Match: []string{}}, // explicitly none
		"e2e":     {Match: []string{"make e2e*", "./scripts/e2e.sh"}},
		"bench":   {Slots: 1, Match: []string{"go test -bench*"}},
	}})
	for argv, want := range map[string]string{
		"go test ./...":        "",
		"make check":           "",
		"make e2e":             "e2e",
		"make e2e-fast V=1":    "e2e",
		"./scripts/e2e.sh":     "e2e",
		"go test -bench=. ./x": "bench",
		"golangci-lint run":    "golangci-lint", // other defaults stay
	} {
		if got := m.Class(strings.Fields(argv)); got != want {
			t.Errorf("Class(%q) = %q, want %q", argv, got, want)
		}
	}
	// Shims go to literal command names only; bench still names go.
	if got := strings.Join(m.Binaries(), " "); got != "dart flutter go golangci-lint make" {
		t.Errorf("Binaries() = %q", got)
	}
}

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		pat, s string
		want   bool
	}{
		{"test*", "test", true}, {"test*", "testing", true}, {"test*", "tes", false},
		{"a?c", "abc", true}, {"a?c", "ac", false}, {"[ab]x", "bx", true}, {"[!ab]x", "bx", false},
		{"check/lint", "check/lint", true}, {"*", "a/b", true}, {"a.b", "axb", false},
	} {
		if got := glob(c.pat, c.s); got != c.want {
			t.Errorf("glob(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}

// shimWorld is a PATH with a fake real tool per name that logs its argv and
// whether it ran under a lease, and a fake saddle that logs `run` calls and
// then runs the command with a lease token, the way saddle run does.
type shimWorld struct {
	dir, shims, real, saddle, log string
}

func newShimWorld(t *testing.T, names ...string) shimWorld {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	d := t.TempDir()
	w := shimWorld{dir: d, shims: filepath.Join(d, "shims"), real: filepath.Join(d, "real"),
		saddle: filepath.Join(d, "saddle"), log: filepath.Join(d, "log")}
	if err := os.MkdirAll(w.real, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		writeExe(t, filepath.Join(w.real, n), "#!/bin/sh\necho \"real $(basename \"$0\") $* lease=${SADDLE_RUNQ_LEASE:-none}\" >> "+shq(w.log)+"\n")
	}
	writeExe(t, w.saddle, "#!/bin/sh\necho \"saddle $*\" >> "+shq(w.log)+"\n"+
		"while [ \"$1\" != -- ]; do shift; done; shift\nSADDLE_RUNQ_LEASE=tok exec \"$@\"\n")
	return w
}

func writeExe(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// path is the PATH an agent pane gets: the shims first.
func (w shimWorld) path(extra ...string) string {
	return strings.Join(append(append([]string{w.shims}, extra...), w.real, "/usr/bin", "/bin"), string(os.PathListSeparator))
}

func (w shimWorld) run(t *testing.T, path string, env []string, argv ...string) string {
	t.Helper()
	_ = os.Remove(w.log)
	// Resolve argv[0] against the shims, not the test machine's PATH: a
	// tool missing there (flutter on CI) would leave exec.Command with a
	// lookup error that overwriting cmd.Path does not clear.
	bin := argv[0]
	if !strings.Contains(bin, "/") {
		bin = filepath.Join(w.shims, bin)
	}
	cmd := exec.Command(bin, argv[1:]...)
	cmd.Env = append([]string{"PATH=" + path}, env...)
	cmd.Dir = w.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out)
	}
	b, _ := os.ReadFile(w.log)
	return strings.TrimSpace(string(b))
}

func TestShimRoutesHeavyAndPassesLight(t *testing.T) {
	w := newShimWorld(t, "go", "flutter")
	names, err := WriteShims(w.shims, w.saddle, NewMatcher(Config{}), w.real)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, " ") != "flutter go" {
		t.Fatalf("shims for %v; want flutter and go only (no golangci-lint, dart or make on this PATH)", names)
	}
	realGo := filepath.Join(w.real, "go")

	if got := w.run(t, w.path(), nil, "go", "test", "./..."); got != "saddle run --class go-test --prio worker -- "+realGo+" test ./...\nreal go test ./... lease=tok" {
		t.Errorf("go test through the shim:\n%s", got)
	}
	for _, argv := range [][]string{{"go", "version"}, {"go", "build", "./..."}, {"flutter", "doctor"}} {
		got := w.run(t, w.path(), nil, argv...)
		if strings.Contains(got, "saddle") || !strings.HasPrefix(got, "real "+argv[0]) {
			t.Errorf("%v should pass straight through, got:\n%s", argv, got)
		}
	}
	// Inside a lease, and with the bypass, heavy tools exec straight away.
	if got := w.run(t, w.path(), []string{"SADDLE_RUNQ_LEASE=outer"}, "go", "test"); got != "real go test lease=outer" {
		t.Errorf("inside a lease: %s", got)
	}
	if got := w.run(t, w.path(), []string{"SADDLE_RUNQ=off"}, "flutter", "test"); got != "real flutter test lease=none" {
		t.Errorf("SADDLE_RUNQ=off: %s", got)
	}
}

// A shim must find the real tool past its own directory, past any other
// saddle shim directory (another repo's panes), and past a PATH that names
// its directory twice or with a trailing slash, without ever running itself.
func TestShimDoesNotRecurse(t *testing.T) {
	w := newShimWorld(t, "go")
	other := filepath.Join(w.dir, "other-shims")
	if _, err := WriteShims(w.shims, w.saddle, NewMatcher(Config{}), w.real); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteShims(other, w.saddle, NewMatcher(Config{}), w.real); err != nil {
		t.Fatal(err)
	}
	path := w.path(w.shims+"/", other, w.shims)
	done := make(chan string, 1)
	go func() { done <- w.run(t, path, []string{"SADDLE_RUNQ_LEASE=x"}, "go", "version") }()
	select {
	case got := <-done:
		if got != "real go version lease=x" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the shim is recursing")
	}
	if p, err := LookReal("go", path, w.shims); err != nil || p != filepath.Join(w.real, "go") {
		t.Fatalf("LookReal = %q, %v", p, err)
	}
	if _, err := LookReal("go", strings.Join([]string{w.shims, other}, ":"), w.shims); err == nil {
		t.Fatal("LookReal found a shim as the real tool")
	}
}

func TestShimRealToolMissing(t *testing.T) {
	w := newShimWorld(t, "go")
	if _, err := WriteShims(w.shims, w.saddle, NewMatcher(Config{}), w.real); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(w.shims, "go"), "version")
	cmd.Env = []string{"PATH=" + w.shims + ":" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 127 || !strings.Contains(string(out), "go: not found") {
		t.Fatalf("err = %v, out = %s; want exit 127 naming the tool", err, out)
	}
}

// WriteShims keeps the directory in step with the config: shims for tools
// no pattern names any more are removed, foreign files are left alone, and
// an unchanged shim isn't rewritten.
func TestWriteShimsSyncs(t *testing.T) {
	w := newShimWorld(t, "go", "flutter")
	if _, err := WriteShims(w.shims, w.saddle, NewMatcher(Config{}), w.real); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(w.shims, "notes.txt")
	writeExe(t, foreign, "mine\n")
	before, _ := os.Stat(filepath.Join(w.shims, "go"))
	m := NewMatcher(Config{Classes: map[string]ClassConfig{
		"flutter-test": {Match: []string{}}, "generic-heavy": {Match: []string{}},
	}})
	names, err := WriteShims(w.shims, w.saddle, m, w.real)
	if err != nil || strings.Join(names, " ") != "go" {
		t.Fatalf("names = %v, %v", names, err)
	}
	if _, err := os.Stat(filepath.Join(w.shims, "flutter")); !os.IsNotExist(err) {
		t.Fatalf("stale flutter shim kept: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("foreign file removed: %v", err)
	}
	after, _ := os.Stat(filepath.Join(w.shims, "go"))
	if !os.SameFile(before, after) {
		t.Fatal("unchanged go shim was rewritten")
	}
}

func TestCheckShims(t *testing.T) {
	w := newShimWorld(t, "go")
	if _, err := WriteShims(w.shims, w.saddle, NewMatcher(Config{}), w.real); err != nil {
		t.Fatal(err)
	}
	if ps := CheckShims(w.shims, w.path()); len(ps) != 0 {
		t.Fatalf("healthy shims reported: %v", ps)
	}
	if ps := CheckShims(w.shims, w.real+":"+w.shims); len(ps) != 1 || !strings.Contains(ps[0], "before") {
		t.Fatalf("PATH order: %v", ps)
	}
	if ps := CheckShims(w.shims, w.shims+":"+t.TempDir()); len(ps) != 1 || !strings.Contains(ps[0], "not found") {
		t.Fatalf("missing real tool: %v", ps)
	}
}
