package mathx

import "testing"

func TestSum(t *testing.T) {
	if got := Sum(2, 3); got != 5 {
		t.Fatalf("Sum = %d", got)
	}
}

func TestMax(t *testing.T) {
	if got := Max(2, 3); got != 3 {
		t.Fatalf("Max = %d", got)
	}
}
