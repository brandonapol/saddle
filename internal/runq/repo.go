package runq

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// RepoLabel names the repo at root for lease status: its base name plus a
// short hash of the path, so two repos both called "demo" stay apart.
func RepoLabel(root string) string {
	if root == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	return filepath.Base(root) + "@" + hex.EncodeToString(sum[:])[:4]
}
