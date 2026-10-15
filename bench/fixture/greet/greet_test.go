package greet

import "testing"

func TestHello(t *testing.T) {
	if got := Hello("Ada"); got != "Hello, Ada!" {
		t.Fatalf("Hello = %q", got)
	}
}

func TestGoodbye(t *testing.T) {
	if got := Goodbye("Ada"); got != "Goodbye, Ada." {
		t.Fatalf("Goodbye = %q", got)
	}
}
