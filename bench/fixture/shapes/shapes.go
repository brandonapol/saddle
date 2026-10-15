// Package shapes measures rectangles and circles.
package shapes

import "math"

// Rect is a W by H rectangle.
type Rect struct{ W, H float64 }

// Area is the rectangle's area.
func (r Rect) Area() float64 {
	return r.W * r.H
}

// Perimeter is the length of the rectangle's edge.
func (r Rect) Perimeter() float64 {
	return 2 * (r.W + r.H)
}

// Circle is a circle of radius R.
type Circle struct{ R float64 }

// Area is the circle's area.
func (c Circle) Area() float64 {
	return math.Pi * c.R * c.R
}

// Perimeter is the circle's circumference.
func (c Circle) Perimeter() float64 {
	return 2 * math.Pi * c.R
}
