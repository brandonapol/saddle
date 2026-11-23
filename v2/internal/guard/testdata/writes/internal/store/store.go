package store

import "os"

// Save may write: store is allowed.
func Save() { _ = os.WriteFile("state.db", nil, 0o600) }
