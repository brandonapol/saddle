package textutil

import "testing"

func TestReverse(t *testing.T) {
	if got := Reverse("abc"); got != "cba" {
		t.Fatalf("Reverse = %q", got)
	}
}

func TestTitle(t *testing.T) {
	if got := Title("hello big world"); got != "Hello Big World" {
		t.Fatalf("Title = %q", got)
	}
}
