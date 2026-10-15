package ledger

// View is the line a statement shows for a posting.
func View(name string, amount int) string {
	return Post(name, amount)
}
