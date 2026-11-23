package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionPrintsTheBuildID(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	got := strings.TrimSpace(out.String())
	if !strings.HasPrefix(got, "saddle2 ") || len(got) <= len("saddle2 ") {
		t.Fatalf("version output %q has no build id", got)
	}
}

func TestUnknownCommandSaysWhatToRun(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"nope"}, &out, &errb); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "usage: saddle2 version") {
		t.Fatalf("stderr %q does not say what to run", errb.String())
	}
}
