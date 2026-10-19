package cli

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/doctor"
)

func TestOrphanTaskCheck(t *testing.T) {
	if r := orphanTaskCheck(nil); r.Status != doctor.OK {
		t.Fatalf("clean = %+v", r)
	}
	r := orphanTaskCheck(map[string][]int{"t33": {4, 5}, "t2": {9}})
	if r.Status != doctor.Warn || !strings.Contains(r.Detail, "3 process") || !strings.Contains(r.Detail, "t2 t33") || r.Fix == "" {
		t.Fatalf("orphans = %+v", r)
	}
}
