package automerge

import "testing"

// #913: in batch mode a tick merges the batch once through Batch, never the
// bottom PR by the merge button, and only while auto-merge is on.
func TestBatchModeReplacesBottomUpMerge(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	calls := 0
	r.w.BatchOn = func() bool { return true }
	r.w.Batch = func() (string, string, error) { calls++; return "pr/t2", "", nil }

	if st := check(t, r.w); calls != 0 || st.Merged != "" {
		t.Fatalf("auto-merge off: batch ran %d times, merged %q", calls, st.Merged)
	}
	r.on(t)
	st := check(t, r.w)
	if calls != 1 || st.Merged != "pr/t2" {
		t.Fatalf("batch ran %d times, merged %q; want once, pr/t2", calls, st.Merged)
	}
	if len(r.gh.merged) != 0 || r.restacks != 0 {
		t.Fatalf("bottom-up merge ran too: merged %v, restacks %d", r.gh.merged, r.restacks)
	}

	r.w.Batch = func() (string, string, error) { return "", "no layer of the batch is green yet", nil }
	if st := check(t, r.w); st.Merged != "" || st.Batch == "" {
		t.Fatalf("nothing green: merged %q, batch %q", st.Merged, st.Batch)
	}
}
