package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestAcquireUpLockRefusesSecondSession(t *testing.T) {
	root := t.TempDir()
	rel, err := acquireUpLock(root, false, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = acquireUpLock(root, false, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--takeover") {
		t.Fatalf("second up: %v", err)
	}
	rel()
	rel, err = acquireUpLock(root, false, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel()
}

func TestUpHasTakeoverFlag(t *testing.T) {
	if upCmd().Flags().Lookup("takeover") == nil {
		t.Fatal("saddle up has no --takeover flag")
	}
}
