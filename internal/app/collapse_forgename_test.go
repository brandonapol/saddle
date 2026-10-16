package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// #667: messages name the repo's forge, GitLab and its MRs on GitLab, and
// keep GitHub's wording on GitHub.
func TestMessagesNameForge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider, name, noun string
	}{
		{config.ForgeGitLab, "GitLab", "MR"},
		{config.ForgeGitHub, "GitHub", "PR"},
	} {
		a, _ := setup(t)
		a.Cfg.Forge.Provider = tc.provider
		if err := a.contains("", ""); err == nil || err.Error() != tc.name+" didn't say which commit the "+tc.noun+" is at" {
			t.Errorf("%s: contains = %v", tc.provider, err)
		}
		tk := store.Task{ID: "t1", Branch: "saddle/t1", PR: "!1"}
		gh006 := errors.New("GH006")
		a.deferQueued(restacked{landedTask: landedTask{Task: tk}}, gh006)
		a.deferQueuedPR(tk, gh006)
		es, err := a.Store.Events(500)
		must(t, err)
		var restack, prs bool
		for _, e := range es {
			restack = restack || strings.Contains(e.Data, "restack left t1's PR !1 and the layers above it as they were: "+tc.name+" refused")
			prs = prs || strings.Contains(e.Data, "unpublished: "+tc.name+" refused the push")
		}
		if !restack || !prs {
			t.Errorf("%s: notices don't name %s (restack %v, prs %v): %+v", tc.provider, tc.name, restack, prs, es)
		}
	}
}
