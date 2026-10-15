package label

import "testing"

func TestDoubled(t *testing.T) {
	if got := Doubled("ada", 3); got != "ada 6" {
		t.Fatalf("Doubled = %q", got)
	}
}
