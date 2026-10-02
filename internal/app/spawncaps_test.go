package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

func TestSpawnDepthCap(t *testing.T) {
	a, _ := setup(t)
	parent := OrchestratorID
	for depth := 1; depth <= DefaultMaxDepth; depth++ {
		c, err := a.Spawn(SpawnReq{Title: "level", Parent: parent})
		if err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		parent = c.ID
	}
	_, err := a.Spawn(SpawnReq{Title: "too deep", Parent: parent})
	if err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("spawn below max depth: err = %v", err)
	}
	if _, err := a.Spawn(SpawnReq{Title: "forced", Parent: parent, Force: true}); err != nil {
		t.Fatalf("force should override the cap: %v", err)
	}
}

func TestSpawnFanOutCap(t *testing.T) {
	a, _ := setup(t)
	a.Cfg.Concurrency = 100
	p, err := a.Spawn(SpawnReq{Title: "parent"})
	must(t, err)
	var kids []store.Task
	for i := 0; i < DefaultMaxChildren; i++ {
		c, err := a.Spawn(SpawnReq{Title: "kid", Parent: p.ID})
		must(t, err)
		kids = append(kids, c)
	}
	_, err = a.Spawn(SpawnReq{Title: "one too many", Parent: p.ID})
	if err == nil || !strings.Contains(err.Error(), "children") {
		t.Fatalf("fan-out over cap: err = %v", err)
	}
	// A finished child frees a slot.
	must(t, a.Store.SetStatus(kids[0].ID, store.Done))
	if _, err := a.Spawn(SpawnReq{Title: "replacement", Parent: p.ID}); err != nil {
		t.Fatalf("slot not freed: %v", err)
	}
}
