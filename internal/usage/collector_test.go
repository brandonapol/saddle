package usage

import "testing"

// #533: an opencode session is read from its database; the collector reports
// each assistant turn once, per provider/model, and later polls add only new
// turns.
func TestCollectorReadsOpenCodeDB(t *testing.T) {
	c := NewCollector()
	if !c.Track(Session{ID: "oc:t1", Task: "t1", Agent: OpenCode, Path: openCodeDB(t), Dir: "/repo/wt"}) {
		t.Fatal("opencode session not tracked")
	}
	bs, err := c.Poll()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Tokens{}
	for _, b := range bs {
		if b.Task != "t1" || b.Adapter != OpenCode {
			t.Fatalf("bucket %+v", b)
		}
		got[b.Model] = b.Tokens
	}
	if len(got) == 0 || got["opencode/big-pickle"].Input != 23238 {
		t.Fatalf("buckets %+v", bs)
	}
	if cur, _ := c.Cursor("oc:t1"); cur.Offset == 0 {
		t.Fatal("cursor shows nothing read")
	}
	if again, err := c.Poll(); err != nil || len(again) != 0 {
		t.Fatalf("second poll %v %v", again, err)
	}
}
