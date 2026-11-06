package prgraph

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMigrateIsIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for range 2 {
		if err := Migrate(t.Context(), db); err != nil {
			t.Fatal(err)
		}
	}
	var v string
	if err := db.QueryRow(`SELECT value FROM prgraph_meta WHERE key = 'schema_version'`).Scan(&v); err != nil || v != "1" {
		t.Fatalf("schema_version = %q, %v", v, err)
	}
	if _, err := db.Exec(`INSERT INTO prgraph_edge(change, dep, kind, source) VALUES('a','b','overlap','x')`); err == nil {
		t.Error("an edge kind outside declared/inferred/verified was accepted")
	}
}

// A forge wait never counts toward a cap or an escalation; a failure does,
// with a capped backoff.
func TestRecordClassifiesOutcomes(t *testing.T) {
	p := DefaultPolicy()
	a := Action{Kind: ActMerge, Change: "t400", PR: 817, Guard: Guard{Head: "ht400"}}
	var att Attempt
	for range 5 {
		att = Record(att, a, Outcome{Kind: OutcomeTransient, Reason: "502"}, t0, p)
	}
	if att.N != 0 || att.Waits != 5 || att.Last != OutcomeTransient {
		t.Errorf("transient: %+v", att)
	}
	att = Record(att, a, Outcome{Kind: OutcomeRateLimited, Until: t0.Add(time.Hour)}, t0, p)
	if !att.NextAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("rate limit: next %v, want the forge's time", att.NextAt)
	}
	for range 10 {
		att = Record(att, a, Outcome{Kind: OutcomeRefused, Reason: "review required"}, t0, p)
	}
	if att.N != 10 || att.Waits != 0 || att.FirstErr != "review required" {
		t.Errorf("refused: %+v", att)
	}
	if got := att.NextAt.Sub(t0); got != p.RetryMax {
		t.Errorf("backoff %v, want the cap %v", got, p.RetryMax)
	}
	if s := Record(att, a, Outcome{Kind: OutcomeStale}, t0, p); s != att {
		t.Errorf("a stale guard changed the attempt: %+v", s)
	}
	// I4: the escalation comes once, and retries continue.
	w := golden1105()
	w.st.Attempts = []Attempt{att}
	w.change("t400").Automerge = true
	p0 := Reconcile(w.input())
	if indexOf(p0, func(x Action) bool { return x.Kind == ActEscalate && x.Key == att.Key }) < 0 {
		t.Fatalf("want an escalation of %s; plan %v", att.Key, p0.Actions)
	}
}

type countLock struct{ n int }

func (l *countLock) Lock(context.Context) (func(), error) {
	l.n++
	return func() {}, nil
}

type failLock struct{}

func (failLock) Lock(context.Context) (func(), error) { return nil, errors.New("busy") }

func TestEngineTicksUnderTheLock(t *testing.T) {
	w := golden1105()
	w.ci = ciGreen
	l := &countLock{}
	e := w.engine()
	e.Locker = l
	done, err := e.Tick(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if l.n != 1 || len(done) == 0 || done[0].Action.Kind != ActMove {
		t.Errorf("lock taken %d times; applied %v", l.n, done)
	}
	e.Locker = failLock{}
	if _, err := e.Tick(t.Context()); err == nil {
		t.Error("a tick ran without the lock")
	}
	if _, err := (Engine{}).Tick(t.Context()); err == nil {
		t.Error("an engine with no store ticked")
	}
}
