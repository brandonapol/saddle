package units

import "testing"

func TestScale(t *testing.T) {
	if got := Scale(3, 2); got != 6 {
		t.Fatalf("Scale = %d", got)
	}
}
