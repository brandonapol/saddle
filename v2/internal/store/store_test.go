package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/brandonapol/saddle/v2/internal/core"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain lets the test binary act as another process that writes to a
// database: see TestProcessesLoseNoUpdate and TestWatchSeesAnotherProcess.
func TestMain(m *testing.M) {
	if dir := os.Getenv("STORE_TEST_WRITER"); dir != "" {
		n, _ := strconv.Atoi(os.Getenv("STORE_TEST_N"))
		if err := bump(dir, n); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// bump adds one to the "counter" setting n times, each a read then a write
// in one transaction.
func bump(dir string, n int) error {
	s, err := Open(dir)
	if err != nil {
		return err
	}
	defer s.Close()
	for i := 0; i < n; i++ {
		err := s.Tx(context.Background(), func(t *Tx) error {
			cur, _, err := t.Setting("counter")
			if err != nil {
				return err
			}
			v, _ := strconv.Atoi(cur.Value)
			return t.Set("counter", strconv.Itoa(v+1), "test")
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func tx(t *testing.T, s *Store, fn func(*Tx) error) {
	t.Helper()
	if err := s.Tx(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

func writer(t *testing.T, dir string, n int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "STORE_TEST_WRITER="+dir, "STORE_TEST_N="+strconv.Itoa(n))
	cmd.Stderr = os.Stderr
	return cmd
}

func TestProcessesLoseNoUpdate(t *testing.T) {
	s := open(t)
	dir := filepath.Dir(s.Path())
	const procs, each = 4, 50
	var wg sync.WaitGroup
	errs := make(chan error, procs)
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- writer(t, dir, each).Run()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("writer process: %v", err)
		}
	}
	tx(t, s, func(x *Tx) error {
		got, _, err := x.Setting("counter")
		if err != nil {
			return err
		}
		if got.Value != strconv.Itoa(procs*each) {
			t.Errorf("counter = %s after %d processes x %d increments, want %d: an update was lost",
				got.Value, procs, each, procs*each)
		}
		return nil
	})
}

func TestWatchSeesAnotherProcessAndNothingElse(t *testing.T) {
	s := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const every = 10 * time.Millisecond
	ch, err := s.Watch(ctx, every)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
		t.Fatal("Watch fired with no write")
	case <-time.After(20 * every):
	}
	if err := writer(t, filepath.Dir(s.Path()), 1).Run(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("Watch did not fire after another process wrote")
	}
	// A read is not a change.
	if _, err := s.Snapshot(ctx, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
		t.Fatal("Watch fired after a read")
	case <-time.After(20 * every):
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("Watch sent after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Watch's channel did not close on cancel")
	}
}

func TestTaskStateAndEventChangeTogether(t *testing.T) {
	s := open(t)
	clock := time.UnixMilli(1_000_000)
	s.Now = func() time.Time { return clock }
	var id int64
	tx(t, s, func(x *Tx) error {
		task, err := x.CreateTask(Task{Title: "store", State: "merged", FixAttempts: 9, Touches: []string{"v2/internal/store/**"}, After: []int64{3}})
		id = task.ID
		return err
	})
	clock = clock.Add(time.Minute)

	// A transaction that fails leaves neither the state nor the event.
	boom := errors.New("boom")
	err := s.Tx(context.Background(), func(x *Tx) error {
		if _, _, err := x.Transition(id, core.Event{Kind: core.SlotGranted}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx returned %v, want boom", err)
	}
	snap, err := s.Snapshot(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Live) != 1 || snap.Live[0].State != "ready" {
		t.Fatalf("after a rolled-back change the task is %+v, want ready", snap.Live)
	}
	cursor := snap.Cursor

	tx(t, s, func(x *Tx) error {
		task, effects, err := x.Transition(id, core.Event{Kind: core.SlotGranted})
		if task.State != "working" || len(effects) != 1 || effects[0] != core.StartTurn {
			t.Errorf("Transition returned %+v %v, want working and start_turn", task, effects)
		}
		return err
	})
	snap, err = s.Snapshot(context.Background(), cursor)
	if err != nil {
		t.Fatal(err)
	}
	got := snap.Live[0]
	if got.State != "working" || got.Since != clock.UnixMilli() || got.Created != 1_000_000 {
		t.Errorf("task = %+v; want working, since moved to the change, created kept", got)
	}
	if len(got.Touches) != 1 || got.Touches[0] != "v2/internal/store/**" || len(got.After) != 1 || got.After[0] != 3 {
		t.Errorf("JSON columns did not round-trip: touches %v after %v", got.Touches, got.After)
	}
	if got.Name() != "t1" {
		t.Errorf("Name() = %q, want t1", got.Name())
	}
	if len(snap.Events) != 1 || snap.Events[0].Kind != "slot_granted" || snap.Events[0].Task != id || snap.Gap {
		t.Errorf("events after the cursor = %+v (gap %v), want exactly the slot_granted event", snap.Events, snap.Gap)
	}

	// Since stays put when only another field changes, and PutTask can't
	// change the state: only Transition can.
	clock = clock.Add(time.Minute)
	tx(t, s, func(x *Tx) error {
		task, _ := x.Task(id)
		task.Session, task.State, task.Reason, task.FixAttempts, task.Held, task.PR = "abc", "merged", "", 5, true, 77
		if err := x.PutTask(task, "session", "abc"); err != nil {
			return err
		}
		if _, _, err := x.Transition(id, core.Event{Kind: core.Message}); err != nil {
			return err
		}
		// An event the state doesn't allow changes nothing and says why.
		var illegal *core.IllegalError
		if _, _, err := x.Transition(id, core.Event{Kind: core.Retry}); !errors.As(err, &illegal) {
			t.Errorf("retry on a working task = %v, want an IllegalError", err)
		}
		if err := x.PutTask(Task{ID: 999, Title: "x"}, "session", ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("PutTask on a missing task = %v, want ErrNotFound", err)
		}
		return nil
	})
	tx(t, s, func(x *Tx) error {
		task, _ := x.Task(id)
		if task.Since != got.Since || task.Session != "abc" {
			t.Errorf("since %d session %q; want since unchanged at %d", task.Since, task.Session, got.Since)
		}
		if task.State != "working" || task.FixAttempts != 0 || task.Held || task.PR != 0 {
			t.Errorf("PutTask changed the state machine's fields: %+v", task)
		}
		_, err := x.Task(999)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Task(999) = %v, want ErrNotFound", err)
		}
		return nil
	})
}

func TestInboxDeliversOnceAndDedups(t *testing.T) {
	s := open(t)
	tx(t, s, func(x *Tx) error {
		task, err := x.CreateTask(Task{Title: "a"})
		if err != nil {
			return err
		}
		for i, m := range []Message{
			{To: task.ID, Sender: "owner", Body: "one"},
			{To: task.ID, Sender: "engine", Body: "ci red", Dedup: "ci:abc"},
			{To: task.ID, Sender: "engine", Body: "ci red again", Dedup: "ci:abc"},
			{To: task.ID, Sender: "owner", Body: "two"},
		} {
			sent, err := x.Send(m)
			if err != nil {
				return err
			}
			if want := i != 2; sent != want {
				t.Errorf("message %d sent = %v, want %v", i, sent, want)
			}
		}
		pending, err := x.Pending(task.ID)
		if err != nil {
			return err
		}
		if len(pending) != 3 || pending[0].Body != "one" || pending[2].Body != "two" {
			t.Fatalf("pending = %+v, want one, ci red, two in order", pending)
		}
		att, err := x.StartAttempt(Attempt{Task: task.ID, Provider: "fake", Argv: []string{"fake", "-p"}})
		if err != nil {
			return err
		}
		// A message that arrives after the turn's prompt was built stays.
		if err := x.Deliver(task.ID, pending[1].ID, att); err != nil {
			return err
		}
		left, err := x.Pending(task.ID)
		if err != nil {
			return err
		}
		if len(left) != 1 || left[0].Body != "two" {
			t.Errorf("after delivering through the second, pending = %+v, want only two", left)
		}
		if ok, err := x.EndAttempt(att, "ok", ""); err != nil || !ok {
			t.Errorf("EndAttempt = %v, %v", ok, err)
		}
		if ok, _ := x.EndAttempt(att, "crash", "late"); ok {
			t.Error("an attempt ended twice")
		}
		return nil
	})
}

func TestQuestionsAnswerOnce(t *testing.T) {
	s := open(t)
	var q int64
	tx(t, s, func(x *Tx) error {
		task, err := x.CreateTask(Task{Title: "a"})
		if err != nil {
			return err
		}
		q, err = x.Ask(task.ID, "per-attempt home?", []string{"yes", "no"})
		return err
	})
	snap, _ := s.Snapshot(context.Background(), 0)
	if len(snap.Questions) != 1 || len(snap.Questions[0].Options) != 2 {
		t.Fatalf("open questions = %+v", snap.Questions)
	}
	tx(t, s, func(x *Tx) error {
		if ok, err := x.Answer(q, "yes", "owner"); err != nil || !ok {
			t.Errorf("Answer = %v, %v", ok, err)
		}
		if ok, err := x.Answer(q, "no", "orchestrator"); err != nil || ok {
			t.Errorf("second Answer = %v, %v; want false, nil", ok, err)
		}
		if _, err := x.Answer(999, "x", "owner"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Answer(999) = %v, want ErrNotFound", err)
		}
		return nil
	})
	if snap, _ = s.Snapshot(context.Background(), 0); len(snap.Questions) != 0 {
		t.Errorf("an answered question is still open: %+v", snap.Questions)
	}
}

func TestRetriesAndProviders(t *testing.T) {
	s := open(t)
	tx(t, s, func(x *Tx) error {
		if err := x.PutRetry(Retry{Key: "push:t1", InputsHash: "h1", Attempts: 1, NextAt: 5, LastError: "lease"}); err != nil {
			return err
		}
		if err := x.PutRetry(Retry{Key: "push:t1", InputsHash: "h1", Attempts: 2, NextAt: 9, LastError: "lease"}); err != nil {
			return err
		}
		r, ok, err := x.Retry("push:t1")
		if err != nil || !ok || r.Attempts != 2 || r.NextAt != 9 {
			t.Errorf("Retry = %+v, %v, %v", r, ok, err)
		}
		return x.PutProvider(Provider{Name: "codex", Available: false, OutUntil: 99, Detail: "usage limit"})
	})
	snap, _ := s.Snapshot(context.Background(), 0)
	if len(snap.Retries) != 1 || len(snap.Providers) != 1 || snap.Providers[0].Available {
		t.Errorf("snapshot retries %+v providers %+v", snap.Retries, snap.Providers)
	}
	tx(t, s, func(x *Tx) error {
		if err := x.ClearRetry("push:t1"); err != nil {
			return err
		}
		if _, ok, _ := x.Retry("push:t1"); ok {
			t.Error("a cleared retry is still there")
		}
		return nil
	})
}

// fill puts tasks tasks (all but live of them ended) and events events in
// the database with single statements, so a large fixture is quick.
func fill(t *testing.T, s *Store, tasks, live, events int) {
	t.Helper()
	series := "WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < ?) "
	if _, err := s.w.Exec(series+`INSERT INTO tasks(title, state, since, created)
		SELECT 'task ' || x, CASE WHEN x <= ? THEN 'working' WHEN x % 2 THEN 'merged' ELSE 'closed' END, x, x FROM c`,
		tasks, live); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(series+"INSERT INTO events(ts, task, kind) SELECT x, 1, 'k' FROM c", events); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotStaysBoundedOnALargeDatabase(t *testing.T) {
	// The bounds don't depend on the size, so the default fixture is small
	// enough for the race detector. STORE_TEST_FULL=1 uses the 500,000
	// events #958 names.
	events := int64(50_000)
	if os.Getenv("STORE_TEST_FULL") != "" {
		events = 500_000
	}
	s := open(t)
	fill(t, s, 5000, 40, int(events))
	ctx := context.Background()

	snap, err := s.Snapshot(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Live) != 40 || len(snap.Ended) != MaxEnded || len(snap.Events) != 0 || snap.Cursor != events {
		t.Fatalf("first snapshot: %d live, %d ended, %d events, cursor %d; want 40, %d, 0, %d",
			len(snap.Live), len(snap.Ended), len(snap.Events), snap.Cursor, MaxEnded, events)
	}
	if snap.Ended[0].ID != 5000 || snap.Ended[MaxEnded-1].ID != 5000-MaxEnded+1 {
		t.Errorf("ended tasks are not the most recent: first t%d last t%d", snap.Ended[0].ID, snap.Ended[MaxEnded-1].ID)
	}

	// A reader 10 events behind gets exactly those.
	snap, err = s.Snapshot(ctx, events-10)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Events) != 10 || snap.Gap || snap.Events[0].ID != events-9 {
		t.Errorf("10 behind: %d events, gap %v", len(snap.Events), snap.Gap)
	}
	// A reader too far behind gets a gap, not a partial history.
	snap, err = s.Snapshot(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Gap || len(snap.Events) != 0 {
		t.Errorf("far behind: %d events, gap %v; want none and a gap", len(snap.Events), snap.Gap)
	}
	// A cursor past the end (another database, or deleted events) is a gap.
	if snap, _ = s.Snapshot(ctx, events+1); !snap.Gap {
		t.Error("a cursor past the newest event is not reported as a gap")
	}
}

func TestSnapshotReportsPrunedEventsAsAGap(t *testing.T) {
	s := open(t)
	now := time.UnixMilli(100 * 24 * time.Hour.Milliseconds())
	s.Now = func() time.Time { return now }
	// Events at day 1..100, one a day; usage rows likewise.
	day := 24 * time.Hour.Milliseconds()
	if _, err := s.w.Exec(`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 100)
		INSERT INTO events(ts, kind) SELECT x * ?, 'k' FROM c`, day); err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 100)
		INSERT INTO usage(minute, task, provider, model) SELECT x * ? / 60000, 1, 'p', 'm' FROM c`, day); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Default retention is 30 days: days 1..69 go from each table.
	if n != 2*69 {
		t.Errorf("Prune removed %d rows, want %d", n, 2*69)
	}
	snap, err := s.Snapshot(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Gap {
		t.Error("events 51..69 were pruned unseen, but the snapshot reports no gap")
	}
	if snap, _ = s.Snapshot(context.Background(), 80); snap.Gap || len(snap.Events) != 20 {
		t.Errorf("cursor inside the retained window: gap %v, %d events; want no gap, 20", snap.Gap, len(snap.Events))
	}

	tx(t, s, func(x *Tx) error { return x.Set(RetainEventsDays, "soon", "test") })
	if _, err := s.Prune(context.Background()); err == nil || !strings.Contains(err.Error(), RetainEventsDays) {
		t.Errorf("Prune with a bad retention setting = %v; want an error naming the setting", err)
	}
}

func TestSnapshotQueriesUseIndexes(t *testing.T) {
	s := open(t)
	fill(t, s, 2000, 20, 20_000)
	if _, err := s.w.Exec("ANALYZE"); err != nil {
		t.Fatal(err)
	}
	args := map[string][]any{
		"live": {MaxLive}, "ended": {MaxEnded}, "questions": {MaxOpen},
		"events": {int64(10), MaxEvents}, "oldest": {}, "pending": {int64(1)},
	}
	for name, q := range snapshotQueries {
		a, ok := args[name]
		if !ok {
			t.Fatalf("query %q has no arguments in this test: add them", name)
		}
		rows, err := s.r.Query("EXPLAIN QUERY PLAN "+q, a...)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			// "SCAN tasks" reads the whole table; "SCAN tasks USING INDEX"
			// walks an index, and "SEARCH" seeks into one.
			if strings.HasPrefix(detail, "SCAN") && !strings.Contains(detail, "USING") {
				t.Errorf("%s reads a whole table: %s\n  %s", name, detail, q)
			}
		}
		_ = rows.Close()
	}
}

func TestMigrateBacksUpAndRefusesANewerDatabase(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tx(t, s, func(x *Tx) error { return x.Set("k", "v", "test") })
	_ = s.Close()

	// A second migration, as a later version would add.
	old := migrations
	migrations = append(append([]string{}, old...), "ALTER TABLE tasks ADD COLUMN extra TEXT NOT NULL DEFAULT ''")
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	tx(t, s, func(x *Tx) error {
		if got, _, _ := x.Setting("k"); got.Value != "v" {
			t.Errorf("data lost in migration: %+v", got)
		}
		return nil
	})
	_ = s.Close()
	baks, _ := filepath.Glob(filepath.Join(dir, DBFile+".bak-schema1-*"))
	if len(baks) != 1 {
		t.Errorf("want one backup of schema 1 before migrating, found %v", baks)
	}

	// The older binary now refuses, and says why.
	migrations = old
	if _, err := Open(dir); !errors.Is(err, ErrNewer) {
		t.Errorf("an older binary opening a newer database = %v, want ErrNewer", err)
	}
}
