package app

import (
	"os"
	"testing"
	"time"
)

func TestModActiveFollowsTheHeartbeat(t *testing.T) {
	a := &App{Root: t.TempDir()}
	now := time.Now()
	if a.ModActive(now) {
		t.Fatal("active with no heartbeat")
	}
	if err := os.MkdirAll(a.stateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.ModHeartbeatPath(), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !a.ModActive(now) {
		t.Fatal("inactive right after a heartbeat")
	}
	if a.ModActive(now.Add(ModFresh + time.Second)) {
		t.Fatal("still active after the heartbeat went stale: a closed session would keep the settings hook off")
	}
}
