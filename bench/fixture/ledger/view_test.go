package ledger

import "testing"

func TestView(t *testing.T) {
	if got := View("ada", 3); got != Post("ada", 3) {
		t.Fatalf("View = %q", got)
	}
}
