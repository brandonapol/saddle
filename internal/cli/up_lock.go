package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/brandonapol/saddle/internal/uplock"
)

// acquireUpLock takes the per-repo saddle up session lock (#665). With
// takeover it stops a live holder instead of refusing.
func acquireUpLock(root string, takeover bool, w io.Writer) (func(), error) {
	path := filepath.Join(root, ".saddle", "up.lock")
	rel, err := uplock.Acquire(path, uplock.Options{Takeover: takeover})
	if err != nil {
		return nil, err
	}
	if takeover {
		fmt.Fprintln(w, "saddle up: took over from any previous session")
	}
	return rel, nil
}
