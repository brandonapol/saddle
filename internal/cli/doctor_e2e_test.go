package cli

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/doctor"
)

// #692: doctor warns with the count of leaked e2e processes, and is quiet
// when there are none.
func TestE2ELeakCheck(t *testing.T) {
	if r := e2eLeakCheck(0, 0); r.Status != doctor.OK {
		t.Errorf("no leaks = %+v, want ok", r)
	}
	r := e2eLeakCheck(3, 2)
	if r.Status != doctor.Warn || !strings.Contains(r.Detail, "3 leaked") || !strings.Contains(r.Detail, "2 abandoned") {
		t.Errorf("3 leaks = %+v, want a warning with both counts", r)
	}
}
