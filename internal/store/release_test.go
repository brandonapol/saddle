package store

import "testing"

// #411: ReleaseN reports how many claims it removed, so a release that
// matched nothing can say so instead of answering "released".
func TestReleaseNCountsRemovedClaims(t *testing.T) {
	s := openTest(t)
	grant := func(globs ...string) ClaimFunc {
		return func(map[string][]string) ([]string, error) { return globs, nil }
	}
	if _, err := s.Claim("t1", grant("a/**", "b.go", "c.go")); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ReleaseN("t2", "a/**"); err != nil || n != 0 {
		t.Fatalf("other task's release = %d, %v; want 0", n, err)
	}
	if n, err := s.ReleaseN("t1", "a/**", "missing"); err != nil || n != 1 {
		t.Fatalf("ReleaseN = %d, %v; want 1", n, err)
	}
	if n, err := s.ReleaseN("t1"); err != nil || n != 2 {
		t.Fatalf("release all = %d, %v; want 2", n, err)
	}
}
