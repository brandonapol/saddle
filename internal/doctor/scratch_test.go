package doctor

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/scratch"
)

// scratchEnv adds a scratch report to the fake.
type scratchEnv struct {
	*fakeEnv
	rep ScratchReport
}

func (e scratchEnv) Scratch() (ScratchReport, error) { return e.rep, nil }

func scratchResult(t *testing.T, env Env) Result {
	t.Helper()
	for _, r := range Run(env) {
		if r.Name == CheckScratch {
			return r
		}
	}
	t.Fatal("no scratch check")
	return Result{}
}

func TestScratchCheck(t *testing.T) {
	base := healthy(t)
	room := scratch.Space{Path: "/c/saddle/tmp", Free: 50, Total: 100}
	for _, tc := range []struct {
		name   string
		rep    ScratchReport
		status Status
		detail string
		fix    string
	}{
		{"healthy", ScratchReport{Root: "/c/saddle/tmp", Bytes: 2048, Files: 3, Space: room, Threshold: 15}, OK, "/c/saddle/tmp: 2.0K in 3 files", ""},
		{"stray /tmp writer", ScratchReport{Root: "/c/saddle/tmp", Space: room, Threshold: 15, Strays: []string{"/tmp/go-build123"}}, Warn, "/tmp/go-build123", "TMPDIR"},
		{"agent TMPDIR outside", ScratchReport{Root: "/c/saddle/tmp", Space: room, Threshold: 15, ProcTmp: "/tmp"}, Warn, "TMPDIR is /tmp", "TMPDIR"},
		{"low", ScratchReport{Root: "/c/saddle/tmp", Space: scratch.Space{Path: "/c/saddle/tmp", Free: 5, Total: 100}, Threshold: 15}, Warn, "spawns are held", "saddle gc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := scratchResult(t, scratchEnv{fakeEnv: base, rep: tc.rep})
			if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) || !strings.Contains(r.Fix, tc.fix) {
				t.Fatalf("%+v", r)
			}
			if r.About == "" {
				t.Error("no about text")
			}
		})
	}
}
