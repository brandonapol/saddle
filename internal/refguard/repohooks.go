package refguard

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
// is left alone; the ones the ref guard owns (pre-push, pre-commit,
// reference-transaction, commit-msg) take the wrapper in their chained slot.
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
		if s.Rel != "" && sameFile(filepath.Join(root, s.Rel), filepath.Join(hooks, s.Name)) {
			continue // core.hooksPath is this directory: the slot is saddle's, not the repo's (#691)
		}
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
	if name == "pre-push" || name == "reference-transaction" || name == "pre-commit" || name == "commit-msg" {
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
		if err := writeHook(h.Path, repoHookScript(h.Shipped)); err != nil {
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
	b.WriteString(recursionGuard(s.Name))
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

// hookDepthEnv counts the saddle hooks running inside one another, as
// ":name:" per hook.
const hookDepthEnv = "SADDLE_HOOK_DEPTH"

// maxHookDepth is how many times one hook name may already be running before
// a saddle script refuses to start another. The ref guard and a repo-hook
// wrapper legitimately stack for one name, and a hook may run git, so the
// limit leaves room; a loop between scripts (#691) still stops in a few
// processes instead of thousands.
const maxHookDepth = 4

// recursionGuard is the shell every script saddle writes starts with: it
// exits when hook name is already nested maxHookDepth deep.
func recursionGuard(name string) string {
	pat := "*"
	for i := 0; i < maxHookDepth; i++ {
		pat += ":" + name + ":*"
	}
	return "case \"$" + hookDepthEnv + "\" in " + pat + ") echo \"saddle: hook recursion in " + name + ", see saddle doctor\" >&2; exit 1 ;; esac\n" +
		hookDepthEnv + "=\"$" + hookDepthEnv + ":" + name + ":\"; export " + hookDepthEnv + "\n"
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

var wrapperTargetRe = regexp.MustCompile(`runs the repo's (\S+) (\S+) hook`)

// cyclic reports whether pre-saddle, a hook's chained slot, is a repo-hook
// wrapper that runs saddle's own ref guard (or the hook it is chained from),
// which would run the wrapper again.
func cyclic(root, hooks, name string) bool {
	b, err := os.ReadFile(filepath.Join(hooks, name+lintgate.ChainSuffix))
	if err != nil || !strings.Contains(string(b), lintgate.RepoHookMarker) {
		return false
	}
	m := wrapperTargetRe.FindStringSubmatch(string(b))
	if m == nil {
		return false
	}
	target := filepath.Join(root, m[1])
	if sameFile(target, filepath.Join(hooks, name)) {
		return true
	}
	tb, err := os.ReadFile(target)
	return err == nil && strings.Contains(string(tb), lintgate.GuardMarker)
}

// Cycles lists the hooks whose chained wrapper runs saddle's ref guard, which
// runs the wrapper again (#691).
func Cycles(root string) ([]string, error) {
	hooks, err := hooksDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range lintgate.GitHooks {
		if cyclic(root, hooks, name) {
			out = append(out, name)
		}
	}
	return out, nil
}

// removeCycles deletes the wrappers Cycles reports; an install from before
// #691 left them behind.
func removeCycles(root, hooks string) error {
	for _, name := range lintgate.GitHooks {
		if cyclic(root, hooks, name) {
			if err := os.Remove(filepath.Join(hooks, name+lintgate.ChainSuffix)); err != nil {
				return err
			}
		}
	}
	return nil
}
