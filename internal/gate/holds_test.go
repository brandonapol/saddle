package gate

import (
	"reflect"
	"testing"
	"time"
)

func TestHoldsAddRemoveList(t *testing.T) {
	var hs Holds
	if err := hs.Add(Hold{ID: "h2", Task: "t3", Until: TaskLanded("t4")}); err != nil {
		t.Fatal(err)
	}
	if err := hs.Add(Hold{ID: "h1", Task: "t3", Until: TaskLanded("t5")}); err != nil {
		t.Fatal(err)
	}
	if err := hs.Add(Hold{ID: "h1", Task: "t3", Until: TaskLanded("t5")}); err == nil {
		t.Error("duplicate id accepted")
	}
	if err := hs.Add(Hold{ID: "h3", Task: "t3"}); err == nil {
		t.Error("hold without condition accepted")
	}
	if err := hs.Add(Hold{Task: "t3", Until: TaskLanded("t5")}); err == nil {
		t.Error("hold without id accepted")
	}
	var ids []string
	for _, h := range hs.List() {
		ids = append(ids, h.ID)
	}
	if !reflect.DeepEqual(ids, []string{"h1", "h2"}) {
		t.Errorf("List ids = %v", ids)
	}
	if _, ok := hs.Get("h2"); !ok {
		t.Error("Get(h2) missing")
	}
	if !hs.Remove("h2") || hs.Remove("h2") {
		t.Error("Remove should succeed once")
	}
	if len(hs.List()) != 1 {
		t.Errorf("List after remove = %v", hs.List())
	}
}

func TestHoldsEvaluateAndRelease(t *testing.T) {
	f := fake{tasks: map[string]TaskInfo{
		"t4": {Status: "running"},
		"t5": {Status: "landed", Done: true, Landed: true},
	}}
	s := f.state()
	var hs Holds
	_ = hs.Add(Hold{ID: "b", Task: "t3", Until: TaskLanded("t4")})
	_ = hs.Add(Hold{ID: "a", Task: "t3", Until: TaskLanded("t5")})

	got := hs.Evaluate(t0, s)
	if len(got) != 2 || got[0].Hold.ID != "a" || !got[0].Ready || got[1].Ready {
		t.Fatalf("Evaluate = %+v", got)
	}
	if got[1].Reason != "waiting on t4 to land (running)" {
		t.Errorf("reason = %q", got[1].Reason)
	}

	rel := hs.Release(t0, s)
	if len(rel) != 1 || rel[0].Hold.ID != "a" || rel[0].Reason != "t5 landed" {
		t.Fatalf("Release = %+v", rel)
	}
	if _, ok := hs.Get("a"); ok {
		t.Error("released hold still registered")
	}
	if _, ok := hs.Get("b"); !ok {
		t.Error("blocked hold removed")
	}
}

func TestHoldsBlocking(t *testing.T) {
	f := fake{tasks: map[string]TaskInfo{"t4": {Status: "running"}, "t6": {Status: "landed", Landed: true}}}
	s := f.state()
	var hs Holds
	_ = hs.Add(Hold{ID: "h1", Task: "t3", Paths: []string{"internal/invoice/**"},
		Until: CommitTouching("t4", t0, "internal/invoice/period.go")})
	_ = hs.Add(Hold{ID: "h2", Task: "t3", Paths: []string{"web/**"}, Until: TaskLanded("t6")})
	_ = hs.Add(Hold{ID: "h3", Task: "t7", Until: TaskLanded("t4")}) // whole task

	if st, ok := hs.Blocking("t3", "internal/invoice/period.go", t0, s); !ok || st.Hold.ID != "h1" {
		t.Fatalf("expected h1 to block, got %+v %v", st, ok)
	} else if got, want := st.Message(), "held (h1): waiting on t4's commit to internal/invoice/period.go"; got != want {
		t.Errorf("Message = %q, want %q", got, want)
	}
	if _, ok := hs.Blocking("t3", "web/a.ts", t0, s); ok {
		t.Error("ready hold h2 should not block")
	}
	if _, ok := hs.Blocking("t3", "README.md", t0, s); ok {
		t.Error("path outside held paths blocked")
	}
	if _, ok := hs.Blocking("t9", "internal/invoice/period.go", t0, s); ok {
		t.Error("other task blocked")
	}
	if st, ok := hs.Blocking("t7", "anything.go", t0, s); !ok || st.Hold.ID != "h3" {
		t.Errorf("hold with no paths should cover every path, got %+v %v", st, ok)
	}
}

func TestHoldsCycles(t *testing.T) {
	var hs Holds
	_ = hs.Add(Hold{ID: "h1", Task: "t1", Until: TaskLanded("t2")})
	_ = hs.Add(Hold{ID: "h2", Task: "t2", Until: Any(TaskDone("t3"), After(t0.Add(time.Hour)))})
	_ = hs.Add(Hold{ID: "h3", Task: "t3", Until: All(TaskLanded("t1"), PRMerged(4))})
	_ = hs.Add(Hold{ID: "h4", Task: "t4", Until: TaskLanded("t1")}) // waits on the cycle, not in it
	_ = hs.Add(Hold{ID: "h5", Task: "t5", Until: TaskLanded("t5")}) // self-wait

	got := hs.Cycles()
	want := [][]string{{"h1", "h2", "h3"}, {"h5"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Cycles = %v, want %v", got, want)
	}
	for range 20 {
		if again := hs.Cycles(); !reflect.DeepEqual(again, got) {
			t.Fatalf("nondeterministic cycles: %v vs %v", again, got)
		}
	}

	var none Holds
	_ = none.Add(Hold{ID: "x", Task: "t1", Until: TaskLanded("t2")})
	if c := none.Cycles(); len(c) != 0 {
		t.Errorf("unexpected cycles %v", c)
	}
}
