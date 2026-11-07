package app

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// seedNoiseEvents bulk-inserts n events unrelated to any repair, as a
// long-lived state.db accumulates them (#778).
func seedNoiseEvents(t *testing.T, a *App, n int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(a.Root, ".saddle", "state.db")+"?_pragma=busy_timeout(10000)")
	must(t, err)
	defer db.Close()
	tx, err := db.Begin()
	must(t, err)
	st, err := tx.Prepare(`INSERT INTO events(ts, task, kind, data) VALUES(?,?,?,?)`)
	must(t, err)
	ts := time.Now().Unix()
	for i := range n {
		if _, err := st.Exec(ts, fmt.Sprintf("n%d", i%500), "tool", "Edit"); err != nil {
			t.Fatal(err)
		}
	}
	must(t, st.Close())
	must(t, tx.Commit())
}

// repairingEntries adds n more repairing train entries alongside t1, each
// with a live repair, so spawnRepairs walks them all without spawning.
func repairingEntries(t *testing.T, a *App, t1, r store.Task, n int) {
	t.Helper()
	var note string
	tr, err := a.Store.Train()
	must(t, err)
	for _, e := range tr {
		if e.Task == t1.ID {
			note = e.Note
		}
	}
	for i := range n {
		id := fmt.Sprintf("r%d", i)
		must(t, a.Store.CreateTask(store.Task{ID: id, Title: id, Role: store.RoleWorker, Status: store.Done}))
		must(t, a.Store.Enqueue(id))
		must(t, a.Store.SetTrain(id, TrainRepairing, note, false))
		a.Store.Event(id, eventRepairNeeded, `{"source":"restack","onto":"main"}`)
		a.Store.Event(id, eventRepairSpawned, r.ID)
	}
}

// #778: repair bookkeeping read the whole events table several times per
// repairing entry, under train.lock. With ten repairing entries and 20k
// unrelated events, spawnRepairs must read only the rows about repairs.
func TestSpawnRepairsReadsBoundedEvents(t *testing.T) {
	t.Parallel()
	a, t1, r := orphanRepair(t)
	repairingEntries(t, a, t1, r, 10)
	seedNoiseEvents(t, a, 20000)

	before := a.Store.EventRowsRead()
	if got := a.spawnRepairs(); len(got) != 0 {
		t.Fatalf("spawnRepairs spawned %v, want none: every entry has a live repair", got)
	}
	if read := a.Store.EventRowsRead() - before; read > 200 {
		t.Fatalf("spawnRepairs read %d event rows for 11 repairing entries, want a bounded handful (not the 20k-row table)", read)
	}
}

// The indexed lookups must keep answering what the full scans did.
func TestRepairLookupsIndexed(t *testing.T) {
	t.Parallel()
	a, t1, r := orphanRepair(t)
	seedNoiseEvents(t, a, 1000)
	if got, ok := a.repairOf(t1.ID); !ok || got.ID != r.ID {
		t.Fatalf("repairOf = %v, %v; want %s", got.ID, ok, r.ID)
	}
	if got, ok := a.repairedBy(r.ID); !ok || got != t1.ID {
		t.Fatalf("repairedBy = %q, %v; want %s", got, ok, t1.ID)
	}
	if got, ok := a.lastEvent(t1.ID, eventRepairSpawned); !ok || got != r.ID {
		t.Fatalf("lastEvent = %q, %v; want %s", got, ok, r.ID)
	}
	if rs := a.repairsSpawned(t1.ID); len(rs) != 1 || rs[0].ID != r.ID {
		t.Fatalf("repairsSpawned = %+v, want [%s]", rs, r.ID)
	}
	if got := a.lastRepairFailure(t1.ID); got != "" {
		t.Fatalf("lastRepairFailure = %q after a spawn, want none", got)
	}
	a.Store.Event(t1.ID, eventRepairFailed, "no slot")
	if got := a.lastRepairFailure(t1.ID); got != "no slot" {
		t.Fatalf("lastRepairFailure = %q, want %q", got, "no slot")
	}
}
