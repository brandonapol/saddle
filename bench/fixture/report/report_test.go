package report

import "testing"

func TestLine(t *testing.T) {
	if got := Line("Ada", 2, 3); got != "Hello, Ada! Your total is 5." {
		t.Fatalf("Line = %q", got)
	}
}
