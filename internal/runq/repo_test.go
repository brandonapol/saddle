package runq

import (
	"strings"
	"testing"
)

// Two repos both named "demo" must get different labels; the same path is stable.
func TestRepoLabelDisambiguates(t *testing.T) {
	a, b := RepoLabel("/home/x/one/demo"), RepoLabel("/home/x/two/demo")
	if a == b {
		t.Fatalf("both repos labelled %q", a)
	}
	if a != RepoLabel("/home/x/one/demo") {
		t.Fatal("label is not stable")
	}
	if !strings.HasPrefix(a, "demo@") || len(a) != len("demo@")+4 {
		t.Fatalf("label %q is not name@hash4", a)
	}
	if RepoLabel("") != "" {
		t.Fatal("empty root must stay empty")
	}
}
