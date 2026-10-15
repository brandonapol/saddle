// Package label formats a scaled value.
package label

import (
	"fmt"

	"benchfixture/units"
)

// Doubled names v scaled by 2.
func Doubled(name string, v int) string {
	return fmt.Sprintf("%s %d", name, units.Scale(v, 2))
}
