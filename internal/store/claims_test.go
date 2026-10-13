package store

import (
	"slices"
	"testing"
)

func heldGlobs(t *testing.T, s *Store, task string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT glob FROM claims WHERE task = ? ORDER BY glob`, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			t.Fatal(err)
		}
		out = append(out, g)
	}
	return out
}

// #292: releasing a path under a broader claim carves it out at once, and
// claiming it back drops the carve-out.
func TestReleaseNCarvesPathOutOfGlob(t *testing.T) {
	s := openTest(t)
	grant := func(globs ...string) ClaimFunc {
		return func(map[string][]string) ([]string, error) { return globs, nil }
	}
	if _, err := s.Claim("t1", grant("dir/**")); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReleaseN("t1", "dir/f.go"); err != nil || n != 1 {
		t.Fatalf("ReleaseN = %d, %v; want 1", n, err)
	}
	if got := heldGlobs(t, s, "t1"); !slices.Equal(got, []string{"!dir/f.go", "dir/**"}) {
		t.Fatalf("claims = %q", got)
	}
	if n, err := s.ReleaseN("t1", "dir/f.go"); err != nil || n != 0 {
		t.Fatalf("second release = %d, %v; want 0", n, err)
	}
	if n, err := s.ReleaseN("t1", "other/f.go"); err != nil || n != 0 {
		t.Fatalf("release outside claims = %d, %v; want 0", n, err)
	}
	if _, err := s.Claim("t1", grant("dir/f.go")); err != nil {
		t.Fatal(err)
	}
	if got := heldGlobs(t, s, "t1"); !slices.Equal(got, []string{"dir/**", "dir/f.go"}) {
		t.Fatalf("claims after reclaim = %q", got)
	}
}
