// Package ledger records named amounts.
package ledger

import "strconv"

// Post records amount under name.
func Post(name string, amount int) string {
	return name + ": " + strconv.Itoa(amount)
}
