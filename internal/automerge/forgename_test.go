package automerge

import (
	"context"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/forge"
)

// providerForge is a Forge that only says its provider.
type providerForge struct{ p string }

func (f providerForge) Provider() string { return f.p }
func (providerForge) PR(context.Context, string, forge.PRFields) (forge.PR, error) {
	return forge.PR{}, nil
}
func (providerForge) MergeSettings(context.Context) (forge.MergeSettings, error) {
	return forge.MergeSettings{}, nil
}
func (providerForge) MergePR(context.Context, string, string, string) error { return nil }

// Hold reasons name the repo's forge: GitLab on GitLab, GitHub otherwise (#667).
func TestRefusalNamesForge(t *testing.T) {
	nodes := []Node{
		{PR: "!1", Error: "boom"},
		{PR: "!2", Base: "main", Mergeable: "UNKNOWN"},
		{PR: "!3", Base: "main", Mergeable: "MERGEABLE", MergeState: "BLOCKED"},
	}
	for _, tc := range []struct {
		gh   GitHub
		want string
	}{
		{&GH{Forge: providerForge{config.ForgeGitLab}}, "GitLab"},
		{&GH{Forge: providerForge{config.ForgeGitHub}}, "GitHub"},
		{&GH{Run: func(...string) (string, error) { return "", nil }}, "GitHub"},
	} {
		w := &Watcher{Base: "main", GH: tc.gh}
		for _, n := range nodes {
			if why, _ := w.refusal(n); !strings.HasPrefix(why, tc.want) {
				t.Errorf("%s refusal(%s) = %q, want it to start with %s", tc.want, n.PR, why, tc.want)
			}
		}
	}
}
