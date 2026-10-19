package automerge

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/forge"
)

// Forge is the part of the forge the watcher uses; forge.Forge is one.
type Forge interface {
	PR(ctx context.Context, ref string, f forge.PRFields) (forge.PR, error)
	MergeSettings(ctx context.Context) (forge.MergeSettings, error)
	MergePR(ctx context.Context, ref, method, head string) error
}

// GH is the watcher's GitHub: the repo's forge.
type GH struct {
	Forge  Forge
	method string // cached once read
}

// Provider is the forge's provider (config.ForgeGitHub when the forge
// doesn't say), so the watcher's reasons name it.
func (g *GH) Provider() string {
	if p, ok := g.Forge.(interface{ Provider() string }); ok {
		return p.Provider()
	}
	return config.ForgeGitHub
}

func (g *GH) forge() Forge { return g.Forge }

// prFields are the fields PR reads.
const prFields = forge.PRURL | forge.PRState | forge.PRDraft | forge.PRMergeable | forge.PRMergeState |
	forge.PRBase | forge.PRHeadBranch | forge.PRHead | forge.PRLabels | forge.PRChecks

func (g *GH) PR(url string) (PR, error) {
	p, err := g.forge().PR(context.Background(), url, prFields)
	if err != nil {
		return PR{}, err
	}
	pr := PR{URL: p.URL, State: p.State, Draft: p.Draft, Mergeable: p.Mergeable, MergeState: p.MergeState,
		Base: p.Base, Head: p.Head, HeadSHA: p.HeadSHA, Labels: p.Labels}
	rollup := make([]Check, len(p.Checks))
	for i, c := range p.Checks {
		rollup[i] = Check{Status: c.Status, Conclusion: c.Conclusion, State: c.State}
	}
	pr.Checks = summarize(rollup)
	if pr.URL == "" {
		pr.URL = url
	}
	return pr, nil
}

// MergeMethod is squash when the repo allows it, else rebase. Saddle needs
// linear history, so it never picks merge commits.
func (g *GH) MergeMethod() (string, error) {
	if g.method != "" {
		return g.method, nil
	}
	s, err := g.forge().MergeSettings(context.Background())
	if err != nil {
		return "", err
	}
	switch {
	case s.Squash:
		g.method = "squash"
	case s.Rebase:
		g.method = "rebase"
	default:
		return "", errors.New("the repo allows neither squash nor rebase merges, and saddle needs linear history")
	}
	return g.method, nil
}

// Merge merges url with method if its head is still head. It never bypasses
// branch protection.
func (g *GH) Merge(url, method, head string) error {
	if !slices.Contains([]string{"squash", "rebase", "merge"}, method) {
		return fmt.Errorf("unknown merge method %q", method)
	}
	return g.forge().MergePR(context.Background(), url, method, head)
}
