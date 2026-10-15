package ledger

import "testing"

func TestPost(t *testing.T) {
	if got := Post("ada", 3); got != "ada: 3" {
		t.Fatalf("Post = %q", got)
	}
}
