package automerge

import (
	"slices"
	"testing"
)

// #652: the tier notice names a GitLab MR "!42" and offers the approve
// command with a ref saddle takes back.
func TestTierNoticeGitLabMR(t *testing.T) {
	got := TierNotice("https://gitlab.com/g/r/-/merge_requests/42", high("auth/token.go matches high glob **/auth/**"))
	want := "MR !42 is high risk (auth/token.go matches high glob **/auth/**); approve with `saddle automerge approve !42`"
	if got != want {
		t.Fatalf("notice:\n got %q\nwant %q", got, want)
	}
}

// #652: approve finds a GitLab MR by "!N", its number or a URL with a page
// after it.
func TestApproveGitLabMRRef(t *testing.T) {
	mr := "https://gitlab.com/g/r/-/merge_requests/"
	for _, ref := range []string{"!2", "2", "#2", mr + "2/diffs"} {
		r := newRig(t, "", "t1", "t2")
		for i, e := range r.entries {
			url := mr + string(rune('1'+i))
			r.gh.prs[url] = r.gh.prs[e.PR]
			delete(r.gh.prs, e.PR)
			r.entries[i].PR = url
		}
		r.tiered(TierLow, map[string]Result{"t2": high("b")})
		prs, err := r.w.Approve(ref)
		if err != nil || !slices.Equal(prs, []string{mr + "2"}) {
			t.Errorf("Approve(%q) = %v, %v; want t2's MR", ref, prs, err)
		}
	}
}
