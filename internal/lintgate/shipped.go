package lintgate

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// RepoHookMarker marks a hook saddle wrote to run one of the repo's own
// hooks in every worktree (#223). Detect looks past it to the repo's hook.
const RepoHookMarker = "# saddle repo hook"

// Shipped is a git hook a repo ships for its developers to install.
type Shipped struct {
	Name string // the git hook, e.g. pre-commit
	// Rel is the script's path from the top of a checkout; "" for lefthook,
	// which runs from its config.
	Rel string
	// Kind is KindHook for a hooks directory, KindHusky or KindLefthook.
	Kind string
	// Source is the file or directory it was found in.
	Source string
}

// Run is the shell command that runs the hook from the top of a checkout,
// passing on the hook's arguments.
func (s Shipped) Run() string {
	switch s.Kind {
	case KindLefthook:
		return "lefthook run " + s.Name + ` "$@"`
	case KindHusky:
		return "sh " + shellQuote(s.Rel) + ` "$@"`
	}
	q := shellQuote(s.Rel)
	return "if [ -x " + q + " ]; then " + q + ` "$@"; else sh ` + q + ` "$@"; fi`
}

// GitHooks are the hooks git runs on the client side.
var GitHooks = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge",
	"pre-push", "post-rewrite", "reference-transaction", "push-to-checkout", "pre-auto-gc",
	"post-index-change",
}

var lefthookHookRe = regexp.MustCompile(`^([a-z-]+):`)

// ShippedHooks lists the git hooks the checkout at root ships: the scripts in
// the first hooks directory that has any (git/hooks, .githooks and the like),
// husky's, or the hooks lefthook's config names. Only names git runs count,
// and samples don't.
func ShippedHooks(root string) []Shipped {
	for _, d := range shippedDirs {
		if out := dirHooks(root, d, KindHook); len(out) > 0 {
			return out
		}
	}
	if out := dirHooks(root, ".husky", KindHusky); len(out) > 0 {
		return out
	}
	if p, ok := first(root, "lefthook.yml", "lefthook.yaml", ".lefthook.yml", ".lefthook.yaml"); ok {
		f, err := os.Open(filepath.Join(root, p))
		if err != nil {
			return nil
		}
		defer f.Close()
		var out []Shipped
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if m := lefthookHookRe.FindStringSubmatch(sc.Text()); m != nil && slices.Contains(GitHooks, m[1]) {
				out = append(out, Shipped{Name: m[1], Kind: KindLefthook, Source: p})
			}
		}
		return out
	}
	return nil
}

func dirHooks(root, dir, kind string) []Shipped {
	es, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		return nil
	}
	var out []Shipped
	for _, e := range es {
		if e.IsDir() || !slices.Contains(GitHooks, e.Name()) {
			continue
		}
		rel := dir + "/" + e.Name()
		out = append(out, Shipped{Name: e.Name(), Rel: rel, Kind: kind, Source: rel})
	}
	return out
}

// isRepoHook reports whether script is a wrapper saddle wrote.
func isRepoHook(script string) bool { return strings.Contains(script, RepoHookMarker) }
