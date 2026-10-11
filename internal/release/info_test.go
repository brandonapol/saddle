package release

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolveUsesStampedValues(t *testing.T) {
	bi := &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "ffffffffffff"}, {Key: "vcs.time", Value: "2000-01-01T00:00:00Z"}}}
	got := Resolve("v0.1.0", "abc1234def", "2026-10-10T12:00:00Z", bi)
	if got.Version != "v0.1.0" || got.Commit != "abc1234def" || got.Date != "2026-10-10T12:00:00Z" {
		t.Fatalf("Resolve = %+v", got)
	}
}

// go build without ldflags still knows its commit and time from the VCS
// stamp; a modified tree is marked dirty.
func TestResolveFallsBackToBuildInfo(t *testing.T) {
	bi := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "0123456789abcdef"},
		{Key: "vcs.time", Value: "2026-10-01T00:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}}
	got := Resolve("dev", "", "", bi)
	if got.Commit != "0123456789abcdef-dirty" || got.Date != "2026-10-01T00:00:00Z" {
		t.Fatalf("Resolve = %+v", got)
	}
	if got := Resolve("dev", "", "", nil); got.Commit != "unknown" || got.Date != "unknown" {
		t.Fatalf("Resolve(nil) = %+v", got)
	}
}

func TestInfoLong(t *testing.T) {
	s := Info{Version: "v0.1.0", Commit: "abc1234def5678", Date: "2026-10-10T12:00:00Z", Schema: 4}.Long()
	for _, want := range []string{"saddle v0.1.0", "commit abc1234def56", "built 2026-10-10T12:00:00Z", "state.db schema 4"} {
		if !strings.Contains(s, want) {
			t.Errorf("Long() = %q, missing %q", s, want)
		}
	}
}
