package shapes

import (
	"math"
	"testing"
)

func TestRect(t *testing.T) {
	r := Rect{W: 2, H: 3}
	if r.Area() != 6 || r.Perimeter() != 10 {
		t.Fatalf("Rect{2,3}: area %v perimeter %v", r.Area(), r.Perimeter())
	}
}

func TestCircle(t *testing.T) {
	c := Circle{R: 1}
	if math.Abs(c.Area()-math.Pi) > 1e-9 || math.Abs(c.Perimeter()-2*math.Pi) > 1e-9 {
		t.Fatalf("Circle{1}: area %v perimeter %v", c.Area(), c.Perimeter())
	}
}
