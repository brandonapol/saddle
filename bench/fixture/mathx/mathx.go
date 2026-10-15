// Package mathx holds small integer helpers.
package mathx

// Sum adds a and b.
func Sum(a, b int) int {
	return a + b
}

// Max is the larger of a and b.
func Max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
