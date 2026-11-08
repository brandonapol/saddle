package soak

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Module is the soak repo's Go module.
const Module = "demo"

// GoMod is the soak repo's go.mod.
const GoMod = "module " + Module + "\n\ngo 1.21\n"

// RedMarker is the file a red-CI change commits in its own package: CI
// fails while any is in the tree. The local gate doesn't look, so the
// change lands and goes red only on the forge, at whatever layer it sits.
const RedMarker = "ci.red"

// GoFile is the path and content of change c's package. It imports the
// package of every change in c.After, so it builds only on a tree that has
// them: a real build dependency.
func GoFile(c Change) (path, body string) {
	var b strings.Builder
	fmt.Fprintf(&b, "// Package %s is soak change %s (issue #%d).\npackage %s\n\n", c.ID, c.ID, c.Issue, c.ID)
	if len(c.After) > 0 {
		b.WriteString("import (\n")
		for _, d := range c.After {
			fmt.Fprintf(&b, "\t%q\n", Module+"/"+d)
		}
		b.WriteString(")\n\n")
	}
	fmt.Fprintf(&b, "// Value is %s's number plus its dependencies'.\nfunc Value() int {\n\treturn %d", c.ID, c.Issue)
	for _, d := range c.After {
		fmt.Fprintf(&b, " + %s.Value()", d)
	}
	b.WriteString("\n}\n")
	return filepath.Join(c.ID, c.ID+".go"), b.String()
}

// ChangeScript is the shell script change c's agent runs in its worktree:
// write the package, commit it once, and for a red-CI change add the
// marker. A killed-worker change first hangs until the harness releases it
// (after killing its window); state is the harness's directory for that.
// The script is idempotent: a resumed agent runs it again.
func ChangeScript(c Change, state string) string {
	path, body := GoFile(c)
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -e\n")
	if c.Has(KilledWorker) {
		rel := filepath.Join(state, "released", c.ID)
		fmt.Fprintf(&b, "if [ ! -e %s ]; then\n\ttouch %s\n\twhile [ ! -e %s ]; do sleep 0.2; done\n\texit 1\nfi\n",
			shq(rel), shq(filepath.Join(state, "hung", c.ID)), shq(rel))
	}
	fmt.Fprintf(&b, "if git log --format=%%s | grep -qx %s; then exit 0; fi\n", shq("soak "+c.ID))
	fmt.Fprintf(&b, "mkdir -p %s\ncat > %s <<'SOAK_EOF'\n%sSOAK_EOF\n", shq(filepath.Dir(path)), shq(path), body)
	if c.Has(RedCI) {
		fmt.Fprintf(&b, "echo %s > %s\n", shq("TestSoak"+strings.ToUpper(c.ID)+" fails on CI"), shq(filepath.Join(c.ID, RedMarker)))
	}
	fmt.Fprintf(&b, "git add -A\ngit commit -q -m %s\n", shq("soak "+c.ID))
	return b.String()
}

// RepairScript is what every agent that isn't a scenario change runs: a
// repair. It removes the red markers on its branch and commits, or commits
// a note when there is nothing to fix, so it always has a commit.
const RepairScript = `#!/bin/sh
set -e
find . -name .git -prune -o -name ` + RedMarker + ` -print | while read -r f; do git rm -q -f "$f"; done
mkdir -p soakrepair
echo "repaired by $(git branch --show-current)" > "soakrepair/$(git branch --show-current | tr / _).txt"
git add -A
git commit -q -m "soak repair"
`

// DispatchScript is the one script every fake agent runs. It finds the
// change from the task's branch (the issue title "cNN ..." makes its
// slug) and runs that change's script; anything else is a repair.
func DispatchScript(state string) string {
	return `#!/bin/sh
set -e
br="$(git branch --show-current)"
c="$(printf '%s\n' "$br" | sed -nE 's#^.*/t[0-9]+-(c[0-9]+)-soak.*$#\1#p')"
if [ -n "$c" ] && [ -f ` + shq(state) + `/changes/"$c".sh ]; then
	exec sh ` + shq(state) + `/changes/"$c".sh
fi
exec sh ` + shq(state) + `/repair.sh
`
}

// IssueTitle is change c's issue title; its slug names the task's branch.
func IssueTitle(c Change) string { return c.ID + " soak change" }

// IssueBody is change c's issue body: its claims, after edges and, for an
// excluded-adapter change, a Provider line naming the excluded adapter.
func IssueBody(c Change, s Scenario, excluded string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write package %s.\n\nclaims: %s/**\n", c.ID, c.ID)
	if len(c.After) > 0 {
		var refs []string
		for _, d := range c.After {
			dc, _ := s.Change(d)
			refs = append(refs, fmt.Sprintf("#%d", dc.Issue))
		}
		fmt.Fprintf(&b, "after: %s\n", strings.Join(refs, ", "))
	}
	if c.Has(ExcludedAdapter) && excluded != "" {
		fmt.Fprintf(&b, "Provider: %s\n", excluded)
	}
	b.WriteString("**Difficulty: mechanical.**\n")
	return b.String()
}

// GradeMarkers is CI's test verdict on a checked-out tree, before flakes:
// "" when it passes, else the failing log. The caller builds the tree.
func GradeMarkers(dir string) string {
	var fails []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case d.IsDir() && d.Name() == ".git":
			return filepath.SkipDir
		case !d.IsDir() && d.Name() == RedMarker:
			body, _ := os.ReadFile(p)
			fails = append(fails, "--- FAIL: "+strings.TrimSpace(string(body)))
		}
		return nil
	})
	sort.Strings(fails)
	return strings.Join(fails, "\n")
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
