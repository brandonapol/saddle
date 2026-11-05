package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/brandonapol/saddle/internal/forge"
)

// #764: the first gh call that hits GitHub's rate limit logs one
// forge_rate_limited event; later calls in the window don't run gh and
// log nothing more.
func TestForgeRateLimitLoggedOncePerWindow(t *testing.T) {
	a, _ := setup(t)
	bin := t.TempDir()
	script := "#!/bin/sh\n[ \"$1 $2\" = \"api rate_limit\" ] && exit 1\necho 'API rate limit exceeded for user ID 1.' >&2\nexit 1\n"
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for range 3 {
		if _, err := a.Forge().PR(context.Background(), "7", forge.PRState); !errors.Is(err, forge.ErrRateLimited) {
			t.Fatalf("PR under the limit = %v; want ErrRateLimited", err)
		}
	}
	es, err := a.Store.Events(1000)
	must(t, err)
	n := 0
	for _, e := range es {
		if e.Kind == EventForgeRateLimited {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d forge_rate_limited events, want 1 per window", n)
	}
}
