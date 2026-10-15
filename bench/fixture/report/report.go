// Package report formats a line built from greet and mathx.
package report

import (
	"fmt"

	"benchfixture/greet"
	"benchfixture/mathx"
)

// Line greets name with the total of a and b.
func Line(name string, a, b int) string {
	return fmt.Sprintf("%s Your total is %d.", greet.Hello(name), mathx.Sum(a, b))
}
