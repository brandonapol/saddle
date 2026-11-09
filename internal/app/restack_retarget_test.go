package app

import (
	"os"
	"path/filepath"
	"testing"
)

// #359: when GitHub can't say where t2's PR points, restack doesn't know
// whether someone moved it, so it doesn't edit a base saddle already set to
// what the layout wants: that edit could only undo a hand retarget, silently.
func TestRestackLeavesBaseItCantSee(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, t2 := mustTask(t, a, "t1"), mustTask(t, a, "t2")
	setPR(t, t1.PR, "OPEN", "main")
	setPR(t, t2.PR, "OPEN", t1.Branch) // the owner moves it by hand
	failPRState(t, a, t2.PR)           // and GitHub won't say so
	moveMain(t, origin, "news.txt")
	before := len(ghLog())

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if got, ok := baseEdits(ghLog()[before:])[t2.PR]; ok {
		t.Fatalf("restack set t2's base to %q without knowing where it pointed", got)
	}
}

// failPRState makes the fake gh fail to report url's state and base, as
// when GitHub rate-limits or times out, while other calls still work.
func failPRState(t *testing.T, a *App, url string) {
	t.Helper()
	bin := filepath.Join(a.Root, ".git", FakeGHSubdir)
	must(t, os.Rename(filepath.Join(bin, "gh"), filepath.Join(bin, "gh.answers")))
	script := `#!/bin/sh
if [ "$1 $2 $3" = "pr view ` + url + `" ] && [ "$5" = "state,mergeable,baseRefName" ]; then
	echo "HTTP 502: Bad Gateway" >&2
	exit 1
fi
exec "` + filepath.Join(bin, "gh.answers") + `" "$@"
`
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
}
