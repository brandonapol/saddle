package app

import (
	"context"
	"fmt"
	"io"

	"github.com/brandonapol/saddle/internal/forge"
)

// EnsureLabels creates saddle's own labels (needs-human, ci-red,
// saddle:ready, saddle-blocker) on the repo's forge, so the first label a PR
// needs never fails for want of it (#787). Best effort: a forge that can't be
// reached or has no label support is reported to out, not an error.
func (a *App) EnsureLabels(out io.Writer) {
	f := a.Forge()
	for _, l := range forge.KnownLabels() {
		if err := f.EnsureLabel(context.Background(), l); err != nil {
			fmt.Fprintf(out, "label %s: not created (%v); saddle creates it on first use\n", l.Name, err)
			return
		}
	}
	fmt.Fprintln(out, "labels: saddle's labels exist on the repo")
}
