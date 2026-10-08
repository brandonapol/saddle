package runq

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// DefaultMatch are the argv patterns that route a command to a class when
// no runq.toml names any for it (docs/runq.md Q1). A class's match in
// config replaces its defaults; match = [] turns them off.
var DefaultMatch = map[string][]string{
	"go-test":       {"go test*", "make check", "make test*"},
	"golangci-lint": {"golangci-lint run*", "make lint*"},
	"flutter-test":  {"flutter test*", "dart test*", "make test-flutter*"},
	"generic-heavy": {"flutter analyze*", "dart analyze*"},
}

// A Matcher maps a command's argv to its heavy-run class.
//
// A pattern is words matched one for one against argv: the first against
// the command's base name, each with shell globs (* ? [...]). A pattern
// whose last word ends in * also takes any further arguments, so "go test*"
// matches `go test -race ./...` while "make check" matches only `make
// check`. When several patterns match, the longest wins ("make test-flutter*"
// over "make test*").
type Matcher struct{ rules []rule }

type rule struct {
	class   string
	pattern string
	words   []*regexp.Regexp
	src     []string
	more    bool // the last word ends in *: extra arguments match too
}

// NewMatcher builds the matcher for c: DefaultMatch with each class c names
// a match for replaced by that list.
func NewMatcher(c Config) Matcher {
	pats := map[string][]string{}
	for class, ps := range DefaultMatch {
		pats[class] = ps
	}
	for class, cc := range c.Classes {
		if cc.Match != nil {
			pats[class] = cc.Match
		}
	}
	var m Matcher
	for class, ps := range pats {
		for _, p := range ps {
			words := strings.Fields(p)
			if len(words) == 0 {
				continue
			}
			r := rule{class: class, pattern: strings.Join(words, " "), src: words, more: strings.HasSuffix(p, "*")}
			for _, w := range words {
				r.words = append(r.words, globRegexp(w))
			}
			m.rules = append(m.rules, r)
		}
	}
	// Longest first, so the first match is the most specific; the shims test
	// in the same order.
	sort.Slice(m.rules, func(i, j int) bool {
		a, b := m.rules[i], m.rules[j]
		if len(a.pattern) != len(b.pattern) {
			return len(a.pattern) > len(b.pattern)
		}
		if a.class != b.class {
			return a.class < b.class
		}
		return a.pattern < b.pattern
	})
	return m
}

// Class is argv's class, or "" for a light command.
func (m Matcher) Class(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	for _, r := range m.rules {
		if r.matches(argv) {
			return r.class
		}
	}
	return ""
}

func (r rule) matches(argv []string) bool {
	if len(argv) < len(r.words) || (!r.more && len(argv) != len(r.words)) {
		return false
	}
	for i, re := range r.words {
		w := argv[i]
		if i == 0 && !strings.Contains(r.src[0], "/") {
			w = filepath.Base(w) // /usr/local/go/bin/go is go; ./scripts/e2e.sh stays a path
		}
		if !re.MatchString(w) {
			return false
		}
	}
	return true
}

// Binaries are the command names patterns start with, the tools a shim can
// stand in for. Paths (./scripts/e2e.sh) and globs are left to the hook.
func (m Matcher) Binaries() []string {
	var out []string
	for _, r := range m.rules {
		if n := r.src[0]; !strings.ContainsAny(n, "/*?[") && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// glob reports whether s matches the shell pattern pat. Unlike path.Match,
// * crosses slashes, as it does in a shell's case.
func glob(pat, s string) bool { return globRegexp(pat).MatchString(s) }

func globRegexp(pat string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	rs := []rune(pat)
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			j := i + 1
			if j < len(rs) && (rs[j] == '!' || rs[j] == '^') {
				j++
			}
			if j < len(rs) && rs[j] == ']' {
				j++
			}
			for j < len(rs) && rs[j] != ']' {
				j++
			}
			if j >= len(rs) { // no closing ]: a literal [
				b.WriteString(`\[`)
				continue
			}
			class := string(rs[i+1 : j])
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile("^" + regexp.QuoteMeta(pat) + "$")
	}
	return re
}

// ShimMarker is the file that marks a directory as saddle's shims. A shim
// looking for its real tool skips every directory holding one, so shims
// from two repos on one PATH never find each other.
const ShimMarker = ".saddle-shims"

// shimTag is in every shim, so WriteShims only ever removes its own files.
const shimTag = "# saddle-runq-shim"

// WriteShims makes dir hold one shim per tool m names that resolves on
// pathEnv (outside shim directories), and removes saddle shims for any
// other tool. A shim execs the real tool at once inside a lease
// (SADDLE_RUNQ_LEASE), under SADDLE_RUNQ=off, or when its argv matches no
// class; otherwise it runs `saddle run --class C --prio worker -- <real>
// args`. It decides in /bin/sh, so the light path costs a shell start and
// never opens the queue. Unchanged shims aren't rewritten. It returns the
// tools shimmed.
func WriteShims(dir, saddleBin string, m Matcher, pathEnv string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	marker := filepath.Join(dir, ShimMarker)
	if err := writeIfChanged(marker, []byte("saddle's heavy-run shims (docs/runq.md Q1(b)); rewritten on every spawn.\n"), 0o644); err != nil {
		return nil, err
	}
	var names []string
	for _, n := range m.Binaries() {
		if _, err := LookReal(n, pathEnv, dir); err != nil {
			continue // the tool isn't installed: a shim would only fail later
		}
		if err := writeIfChanged(filepath.Join(dir, n), []byte(shimScript(n, dir, saddleBin, m)), 0o755); err != nil {
			return names, err
		}
		names = append(names, n)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return names, err
	}
	for _, e := range ents {
		if e.IsDir() || slices.Contains(names, e.Name()) || !isShim(filepath.Join(dir, e.Name())) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return names, err
		}
	}
	return names, nil
}

// writeIfChanged replaces p with b through a rename, so a shim running
// while it is rewritten reads either version whole.
func writeIfChanged(p string, b []byte, mode os.FileMode) error {
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, b) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), mode)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

func isShim(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 256)
	n, _ := f.Read(head)
	return bytes.Contains(head[:n], []byte(shimTag))
}

// LookReal finds the real name on pathEnv the way a shim does: skipping
// shimDir and every directory with a ShimMarker, and any file that is a
// saddle shim, so a shim never runs itself.
func LookReal(name, pathEnv, shimDir string) (string, error) {
	shimDir = filepath.Clean(shimDir)
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" {
			d = "."
		}
		if filepath.Clean(d) == shimDir {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, ShimMarker)); err == nil {
			continue
		}
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 && !isShim(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found on PATH outside %s", name, shimDir)
}

// CheckShims reports what is wrong with dir's shims on pathEnv: a shim
// whose real tool is gone, a PATH where the real tool comes first, or a
// PATH without dir at all.
func CheckShims(dir, pathEnv string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return []string{fmt.Sprintf("shim dir %s: %v", dir, err)}
	}
	var out []string
	onPath := false
	for _, d := range filepath.SplitList(pathEnv) {
		if filepath.Clean(d) == filepath.Clean(dir) {
			onPath = true
		}
	}
	if !onPath {
		out = append(out, dir+" is not on PATH")
	}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() || !isShim(p) {
			continue
		}
		real, err := LookReal(e.Name(), pathEnv, dir)
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		if onPath && firstOnPath(e.Name(), pathEnv) != p {
			out = append(out, fmt.Sprintf("%s comes before the %s shim on PATH, so its heavy runs skip the queue", real, e.Name()))
		}
	}
	return out
}

func firstOnPath(name, pathEnv string) string {
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" {
			d = "."
		}
		p := filepath.Join(filepath.Clean(d), name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// shimScript is the /bin/sh shim for name. Its pattern tests mirror
// Matcher.Class: the same rules in the same order, first match wins.
func shimScript(name, dir, saddleBin string, m Matcher) string {
	var match strings.Builder
	for _, r := range m.rules {
		if r.src[0] != name {
			continue
		}
		args := r.src[1:]
		test := fmt.Sprintf("[ $# -eq %d ]", len(args))
		if r.more {
			test = fmt.Sprintf("[ $# -ge %d ]", len(args))
		}
		inner := "_saddle_class=" + shellQuote(r.class)
		for i := len(args) - 1; i >= 0; i-- {
			inner = fmt.Sprintf("case \"$%d\" in %s) %s;; esac", i+1, casePattern(args[i]), inner)
		}
		fmt.Fprintf(&match, "\tif [ -z \"$_saddle_class\" ] && %s; then %s; fi # %s\n", test, inner, r.pattern)
	}
	return fmt.Sprintf(`#!/bin/sh
%[1]s for %[2]s: routes heavy %[2]s runs through saddle's heavy-run queue
# (docs/runq.md Q1(b)). Generated by saddle and rewritten on every spawn;
# SADDLE_RUNQ=off bypasses it.
_saddle_real=
_saddle_ifs=$IFS
set -f
IFS=:
for _saddle_d in $PATH; do
	[ -n "$_saddle_d" ] || _saddle_d=.
	[ "$_saddle_d" = %[3]s ] && continue
	[ -e "$_saddle_d/%[4]s" ] && continue
	if [ -f "$_saddle_d/%[2]s" ] && [ -x "$_saddle_d/%[2]s" ]; then
		_saddle_real=$_saddle_d/%[2]s
		break
	fi
done
IFS=$_saddle_ifs
set +f
if [ -z "$_saddle_real" ]; then
	echo "saddle shim: %[2]s: not found on PATH outside "%[3]s >&2
	exit 127
fi
if [ -z "$SADDLE_RUNQ_LEASE" ] && [ "$SADDLE_RUNQ" != off ] && [ -x %[5]s ]; then
	_saddle_class=
%[6]s	if [ -n "$_saddle_class" ]; then
		exec %[5]s run --class "$_saddle_class" --prio worker -- "$_saddle_real" "$@"
	fi
fi
exec "$_saddle_real" "$@"
`, shimTag, name, shellQuote(dir), ShimMarker, shellQuote(saddleBin), match.String())
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// casePattern escapes a pattern word for a shell case, keeping its globs.
func casePattern(w string) string {
	var b strings.Builder
	for _, c := range w {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("*?[]!._/-=+:,@%^", c):
			b.WriteRune(c)
		default:
			b.WriteRune('\\')
			b.WriteRune(c)
		}
	}
	return b.String()
}
