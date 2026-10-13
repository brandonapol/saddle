package app

import "testing"

// #413: t1 queued first is held while t2 lands, then released and landed on
// top of t2. The stack follows landing order, so prs sees each layer's own
// work and restack doesn't refuse t1's commit as unowned.
func TestStackFollowsLandingOrderAfterHoldRelease(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	queueTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	must(t, a.Hold("t1", "waits for t2"))
	_, err := a.Land()
	must(t, err)
	must(t, a.Unhold("t1"))
	_, err = a.Land()
	must(t, err)

	ls, err := a.StackLayers()
	must(t, err)
	if len(ls) != 2 || ls[0].Task.ID != "t2" || ls[1].Task.ID != "t1" {
		t.Fatalf("layers = %+v, want t2 then t1", ls)
	}
	for _, l := range ls {
		if l.Problem != "" {
			t.Errorf("%s: %s", l.Task.ID, l.Problem)
		}
	}
	moveMain(t, origin, "news.txt")
	if _, err := a.Restack(); err != nil {
		t.Fatalf("restack refused after hold/release: %v", err)
	}
	assertStackKeeps(t, a, map[string]string{"t1": "one", "t2": "two"})
}
