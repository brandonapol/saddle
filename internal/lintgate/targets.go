package lintgate

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// makefiles are the names GNU make reads, in the order it looks for them.
var makefiles = []string{"GNUmakefile", "makefile", "Makefile"}

// hasMakefile reports whether root has a makefile make would read.
func hasMakefile(root string) bool {
	_, ok := first(root, makefiles...)
	return ok
}

// hasTarget reports whether the makefile at root, or a file it includes,
// has a rule for target. `target := value` is a variable, and a name only
// listed under .PHONY has no rule.
func hasTarget(root, target string) bool {
	return slices.Contains(makeTargets(root), target)
}

// makeTargets lists the explicit targets of the makefile at root, following
// include lines whose paths are literal.
func makeTargets(root string) []string {
	name, ok := first(root, makefiles...)
	if !ok {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	var read func(path string, depth int)
	read = func(path string, depth int) {
		if seen[path] || depth > 8 {
			return
		}
		seen[path] = true
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		for _, t := range parseTargets(string(b)) {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
		for _, inc := range includes(string(b)) {
			if !filepath.IsAbs(inc) {
				inc = filepath.Join(root, inc)
			}
			read(inc, depth+1)
		}
	}
	read(filepath.Join(root, name), 0)
	return out
}

var (
	includeRe = regexp.MustCompile(`^[-s]?include\s+(.*)$`)
	// targetVarRe is the start of a target-specific variable: `t: VAR = x`.
	targetVarRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*\s*(?:::|[:+?!])?=`)
	directives  = []string{"include", "-include", "sinclude", "ifeq", "ifneq", "ifdef", "ifndef", "else", "endif",
		"export", "unexport", "override", "private", "vpath", "undefine"}
)

// includes are the literal paths a makefile includes.
func includes(src string) []string {
	var out []string
	for _, line := range logicalLines(src) {
		m := includeRe.FindStringSubmatch(strings.TrimSpace(stripComment(line)))
		if m == nil {
			continue
		}
		for _, f := range strings.Fields(m[1]) {
			if !strings.ContainsAny(f, "$%*?[") {
				out = append(out, f)
			}
		}
	}
	return out
}

// parseTargets lists the explicit targets a makefile defines rules for, in
// order: names left of a rule's colon. It skips recipe lines, define
// blocks, variable assignments (including target-specific ones), special
// targets like .PHONY, and pattern rules. Help text after ## is a comment.
func parseTargets(src string) []string {
	var out []string
	inDefine := 0
	for _, line := range logicalLines(src) {
		if strings.HasPrefix(line, "\t") {
			continue // a recipe line
		}
		line = strings.TrimSpace(stripComment(line))
		word, _, _ := strings.Cut(line, " ")
		switch {
		case line == "":
			continue
		case word == "define" || strings.HasPrefix(line, "define\t"):
			inDefine++
			continue
		case word == "endef":
			if inDefine > 0 {
				inDefine--
			}
			continue
		case inDefine > 0:
			continue
		case slices.Contains(directives, word):
			continue
		}
		i := strings.IndexByte(line, ':')
		if i < 0 || strings.ContainsRune(line[:i], '=') {
			continue // no rule, or a variable whose value holds a colon
		}
		rest := strings.TrimPrefix(line[i+1:], ":")
		if strings.HasPrefix(rest, "=") || targetVarRe.MatchString(strings.TrimSpace(rest)) {
			continue // := or ::=, or a target-specific variable
		}
		for _, t := range strings.Fields(line[:i]) {
			if strings.HasPrefix(t, ".") || strings.ContainsAny(t, "%$") || slices.Contains(out, t) {
				continue
			}
			out = append(out, t)
		}
	}
	return out
}

// logicalLines splits a makefile into lines, joining backslash
// continuations.
func logicalLines(src string) []string {
	var out []string
	cur := ""
	for _, l := range strings.Split(src, "\n") {
		l = strings.TrimSuffix(l, "\r")
		if strings.HasSuffix(l, "\\") {
			cur += strings.TrimSuffix(l, "\\") + " "
			continue
		}
		out = append(out, cur+l)
		cur = ""
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// stripComment drops a makefile comment: from an unescaped # on.
func stripComment(line string) string {
	for i := 0; i < len(line); i++ {
		if line[i] == '\\' {
			i++
			continue
		}
		if line[i] == '#' {
			return line[:i]
		}
	}
	return line
}

var (
	targetNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_/.-]*$`)
	// shellWords are words a make target in a hook can't be: a bare `make`
	// followed by a block terminator once came out as `make fi` (#228).
	shellWords = []string{"if", "then", "else", "elif", "fi", "for", "while", "until", "do", "done", "case", "esac", "in",
		"exit", "return", "exec", "true", "false", "echo"}
	// makeArgFlags take the next word as their value.
	makeArgFlags = []string{"-o", "-W", "-I", "-l", "--old-file", "--what-if", "--include-dir", "--load-average"}
	// makeElsewhere point make at another makefile or directory, so the
	// target can't be checked against, or run from, the top of the repo.
	makeElsewhere = []string{"-C", "-f", "--directory", "--file", "--makefile"}
)

// makeTargetsIn returns the target of each `make` invocation in one line of
// shell, in order. An invocation without a target, or one run against
// another directory or makefile, contributes nothing.
func makeTargetsIn(line string) []string {
	for _, sep := range []string{"&&", "||", ";", "|", "&", "(", ")", "{", "}", "`"} {
		line = strings.ReplaceAll(line, sep, " ; ")
	}
	words := strings.Fields(line)
	var out []string
	for i := 0; i < len(words); i++ {
		if words[i] != "make" || (i > 0 && !commandStart(words[:i])) {
			continue
		}
		if t, ok := makeTarget(words[i+1:]); ok {
			out = append(out, t)
		}
	}
	return out
}

// commandStart reports whether the word after before begins a command: at
// the start of the line, after a separator, or after exec and the like.
func commandStart(before []string) bool {
	switch before[len(before)-1] {
	case ";", "exec", "command", "time", "nice", "then", "do", "else", "!":
		return true
	}
	return false
}

// makeTarget is the first target in make's arguments.
func makeTarget(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == ";":
			return "", false
		case slices.Contains(makeElsewhere, a) || hasFlagPrefix(a, makeElsewhere):
			return "", false
		case slices.Contains(makeArgFlags, a):
			i++
		case a == "-j" || a == "--jobs":
			if i+1 < len(args) && isNumber(args[i+1]) {
				i++
			}
		case strings.HasPrefix(a, "-"):
		case strings.Contains(a, "="):
			// VAR=value on make's command line
		case targetNameRe.MatchString(a) && !slices.Contains(shellWords, a):
			return a, true
		default:
			return "", false
		}
	}
	return "", false
}

// hasFlagPrefix reports whether a is one of flags with its value attached:
// -Cdir, --file=x.
func hasFlagPrefix(a string, flags []string) bool {
	for _, f := range flags {
		if strings.HasPrefix(f, "--") && strings.HasPrefix(a, f+"=") {
			return true
		}
		if !strings.HasPrefix(f, "--") && strings.HasPrefix(a, f) {
			return true
		}
	}
	return false
}

func isNumber(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}
