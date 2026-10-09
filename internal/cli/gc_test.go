package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/scratch"
)

func TestWriteScratchSweep(t *testing.T) {
	t.Parallel()
	res := scratch.Result{
		Removed: []scratch.Entry{{Path: "/tmp/go-build1", Bytes: 2048}},
		Kept: []scratch.Entry{
			{Path: "/tmp/saddle-busy", Reason: "held by a live process"},
			{Path: "/tmp/go-build2", Reason: "fresh: changed 1m0s ago"},
		},
		Freed: 2048,
	}
	var dry, real bytes.Buffer
	writeScratchSweep(&dry, res, true)
	writeScratchSweep(&real, res, false)
	for _, want := range []string{"leftover scratch  /tmp/go-build1 (2.0K)", "kept     scratch  /tmp/saddle-busy (held by a live process)", "would free 2.0K"} {
		if !strings.Contains(dry.String(), want) {
			t.Errorf("dry run lacks %q:\n%s", want, dry.String())
		}
	}
	if !strings.Contains(real.String(), "removed  scratch  /tmp/go-build1") || strings.Contains(real.String(), "go-build2") {
		t.Errorf("sweep output:\n%s", real.String())
	}
}
