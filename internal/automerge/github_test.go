package automerge

import (
	"slices"
	"strings"
	"testing"
)

func TestGHReadsPR(t *testing.T) {
	var calls [][]string
	g := &GH{Run: func(args ...string) (string, error) {
		calls = append(calls, args)
		return `{"url":"u","state":"OPEN","isDraft":true,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN",
			"baseRefName":"main","headRefName":"saddle/t1","headRefOid":"abc",
			"labels":[{"name":"needs-human"}],
			"statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"},{"status":"QUEUED"}]}`, nil
	}}
	pr, err := g.PR("u")
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Draft || pr.Base != "main" || pr.HeadSHA != "abc" || !slices.Equal(pr.Labels, []string{NeedsHuman}) || pr.Checks != ChecksPending {
		t.Fatalf("pr = %+v", pr)
	}
	if !strings.Contains(strings.Join(calls[0], " "), "statusCheckRollup") {
		t.Fatalf("call = %v", calls[0])
	}
}

func TestGHMergeMethodAndMerge(t *testing.T) {
	var calls []string
	settings := `{"allow_squash_merge":false,"allow_rebase_merge":true,"allow_merge_commit":true}`
	g := &GH{Run: func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return settings, nil
	}}
	m, err := g.MergeMethod()
	if err != nil || m != "rebase" {
		t.Fatalf("method = %q, %v; want rebase (never merge commits)", m, err)
	}
	if err := g.Merge("u", m, "abc"); err != nil {
		t.Fatal(err)
	}
	last := calls[len(calls)-1]
	if last != "pr merge u --rebase --match-head-commit abc" || strings.Contains(last, "--admin") {
		t.Fatalf("merge call = %q", last)
	}
	g2 := &GH{Run: func(...string) (string, error) {
		return `{"allow_squash_merge":false,"allow_rebase_merge":false}`, nil
	}}
	if _, err := g2.MergeMethod(); err == nil {
		t.Fatal("merge-commit-only repo: want error")
	}
}
