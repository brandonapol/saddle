package release

import (
	"strings"
	"testing"
	"time"
)

func TestUpRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	info := Info{Version: "v0.1.0", Commit: "abc"}
	remove, err := WriteUpRecord(dir, info, 4242, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := ReadUpRecord(dir)
	if err != nil || !ok || rec.PID != 4242 || rec.Version != "v0.1.0" || rec.Commit != "abc" {
		t.Fatalf("ReadUpRecord = %+v, %v, %v", rec, ok, err)
	}
	remove()
	if _, ok, _ := ReadUpRecord(dir); ok {
		t.Fatal("record left after remove")
	}
}

// t106 sat on a stale binary: saddle up kept running the old version after
// an upgrade. The skew check names both versions and says to restart (#326).
func TestSkewWarningForStaleUp(t *testing.T) {
	alive := func(int) bool { return true }
	rec := UpRecord{PID: 7, Version: "v0.1.0", Commit: "abc"}
	w := SkewWarning(rec, Info{Version: "v0.2.0", Commit: "def"}, alive)
	for _, want := range []string{"v0.1.0", "v0.2.0", "pid 7", "restart"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning %q missing %q", w, want)
		}
	}
	// Dev builds compare commits.
	if w := SkewWarning(UpRecord{PID: 7, Version: "abc", Commit: "abc"}, Info{Version: "def", Commit: "def"}, alive); w == "" {
		t.Error("no warning for a dev build on another commit")
	}
}

func TestSkewWarningQuiet(t *testing.T) {
	alive := func(int) bool { return true }
	cur := Info{Version: "v0.2.0", Commit: "def"}
	for name, tc := range map[string]struct {
		rec   UpRecord
		alive func(int) bool
	}{
		"same version":  {UpRecord{PID: 7, Version: "v0.2.0", Commit: "def"}, alive},
		"up is newer":   {UpRecord{PID: 7, Version: "v0.3.0", Commit: "zzz"}, alive},
		"up not alive":  {UpRecord{PID: 7, Version: "v0.1.0", Commit: "abc"}, func(int) bool { return false }},
		"no record pid": {UpRecord{}, alive},
	} {
		if w := SkewWarning(tc.rec, cur, tc.alive); w != "" {
			t.Errorf("%s: warning %q, want none", name, w)
		}
	}
}
