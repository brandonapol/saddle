package refguard

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/lintgate"
)

// Repo hooks (#223). A repo that ships hooks (git/hooks/pre-commit running
// make check, a commit-msg that rejects AI attribution) expects each
// developer to install them; agent worktrees never did, so neither gate ran
// on any agent commit. saddle init installs a small wrapper per shipped hook
// into the shared hooks directory, so every worktree runs the hook, and the
// worktree's own copy of it. A hook already there that saddle didn't write
// is left alone; the ones the ref guard owns (pre-push,
// reference-transaction) take the wrapper in their chained slot.
//
// Many agents committing at once would run many make checks at once and
// collide (golangci-lint: "parallel golangci-lint is running"), so the
// heavy hooks run one at a time across worktrees, under a lock in the common
// git dir. A tree that just passed is not checked again when
// SADDLE_FAST_HOOK=1, and the merge train (SADDLE_TRAIN=1), which runs the
// repo's gate itself, skips them.

// heavyHooks run the repo's checks, so they queue and take the fast path.
var heavyHooks = []string{"pre-commit", "pre-merge-commit", "pre-push"}

// RepoHook is one hook the repo ships and whether it runs.
type RepoHook struct {
	lintgate.Shipped
	// Path is where git looks for it in the shared hooks directory; for a
	// hook the ref guard owns, the slot its hook chains.
	Path string
	// Active is set when something runs there: saddle's wrapper, or a hook
	// someone else installed (make setup/hooks, say).
	Active bool
	// Wrapper is set when saddle's wrapper is installed at Path.
	Wrapper bool
}

// RepoHooks reports each hook the checkout at root ships and whether git
// runs it in every worktree.
func RepoHooks(root string) ([]RepoHook, error) {
	shipped := lintgate.ShippedHooks(root)
	if len(shipped) == 0 {
		return nil, nil
	}
	hooks, err := hooksDir(root)
	if err != nil {
		return nil, err
	}
	var out []RepoHook
	for _, s := range shipped {
		h := RepoHook{Shipped: s, Path: repoHookPath(hooks, s.Name)}
		if b, err := os.ReadFile(h.Path); err == nil {
			h.Active = true
			h.Wrapper = strings.Contains(string(b), lintgate.RepoHookMarker)
		}
		out = append(out, h)
	}
	return out, nil
}

// repoHookPath is where saddle installs the wrapper for hook name.
func repoHookPath(hooks, name string) string {
	p := filepath.Join(hooks, name)
	if name == "pre-push" || name == "reference-transaction" {
		p += lintgate.ChainSuffix
	}
	return p
}

// InstallRepoHooks writes a wrapper for each hook the repo at root ships
// that isn't active yet, and rewrites saddle's own wrappers. It returns the
// hooks it installed.
func InstallRepoHooks(root string) ([]string, error) {
	hs, err := RepoHooks(root)
	if err != nil || len(hs) == 0 {
		return nil, err
	}
	var done []string
	for _, h := range hs {
		if h.Active && !h.Wrapper {
			continue // someone else's hook runs there
		}
		if err := os.MkdirAll(filepath.Dir(h.Path), 0o755); err != nil {
			return done, err
		}
		_ = os.Remove(h.Path)
		if err := os.WriteFile(h.Path, []byte(repoHookScript(h.Shipped)), 0o755); err != nil {
			return done, err
		}
		done = append(done, h.Name)
	}
	return done, nil
}

// repoHookScript is the wrapper that runs s from the top of the worktree
// git runs it in.
func repoHookScript(s lintgate.Shipped) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#!/bin/sh\n%s: runs the repo's %s %s hook in every worktree.\n", lintgate.RepoHookMarker, s.Source, s.Name)
	b.WriteString("# Written by saddle init; reinstalling overwrites it. Delete it to stop.\n")
	b.WriteString("# The merge train runs the repo's gate itself.\n")
	b.WriteString("[ \"$SADDLE_TRAIN\" = 1 ] && exit 0\n")
	b.WriteString("top=$(git rev-parse --show-toplevel) || exit 0\n")
	b.WriteString("cd \"$top\" || exit 0\n")
	if s.Rel != "" {
		fmt.Fprintf(&b, "[ -f %s ] || exit 0\n", shellQuote(s.Rel))
	} else {
		b.WriteString("command -v lefthook >/dev/null 2>&1 || { echo \"saddle: lefthook isn't installed, so the repo's " + s.Name + " hook was skipped\" >&2; exit 0; }\n")
	}
	if !slices.Contains(heavyHooks, s.Name) {
		fmt.Fprintf(&b, "%s\n", s.Run())
		return b.String()
	}
	b.WriteString(`common=$(git rev-parse --path-format=absolute --git-common-dir) || exit 1
state="$common/saddle-hooks"
mkdir -p "$state"
stamp="$state/` + s.Name + `.ok"
tree=$(git write-tree 2>/dev/null)
if [ "$SADDLE_FAST_HOOK" = 1 ] && [ -n "$tree" ] && [ -f "$stamp" ] && grep -qx "$tree" "$stamp"; then
	echo "saddle: tree $tree already passed the repo's ` + s.Name + ` hook (SADDLE_FAST_HOOK=1)" >&2
	exit 0
fi
# One heavy hook at a time across every worktree.
lock="$state/lock"
if command -v flock >/dev/null 2>&1; then
	exec 9>"$lock"
	flock 9
else
	until mkdir "$lock.d" 2>/dev/null; do
		pid=$(cat "$lock.d/pid" 2>/dev/null)
		if [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; then rm -rf "$lock.d"; continue; fi
		sleep 1
	done
	echo $$ > "$lock.d/pid"
	trap 'rm -rf "$lock.d"' EXIT INT TERM
fi
`)
	// fd 9 holds the lock: a daemon the check leaves running mustn't keep it.
	fmt.Fprintf(&b, "{ %s; } 9>&- || exit $?\n", s.Run())
	b.WriteString(`if [ -n "$tree" ]; then
	{ tail -n 199 "$stamp" 2>/dev/null; echo "$tree"; } > "$stamp.tmp" && mv "$stamp.tmp" "$stamp"
fi
`)
	return b.String()
}
