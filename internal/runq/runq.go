// Package runq is a prototype of the heavy-run scheduler (#236): a
// cross-process lease queue that makes CPU-heavy commands (flutter test,
// golangci-lint, make check) from many agents, sessions and repos take turns.
// See docs/runq.md for the design.
//
// The queue lives in one user-level SQLite file. Each class of run (go-test,
// flutter-test, ...) has a slot count. A contender inserts a waiting row and
// promotes itself to running when its rank among the class's waiters fits in
// the free slots. Ranks are priority (aged by wait time, so nothing starves)
// then arrival order. Every lease also holds an flock on its own lock file;
// the kernel drops that lock the moment the process dies, so any other
// contender can reap a dead holder on its next poll without trusting PIDs.
// Heartbeats are the fallback for holders the flock can't vouch for.
//
// Nothing outside this package uses it yet.
package runq

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	_ "modernc.org/sqlite"
)

// Environment variables.
const (
	// EnvLease carries a held lease's token to child processes. A contender
	// that finds a live lease here runs nested inside it instead of queueing,
	// so a pre-commit hook's `make check` inside a held run can't deadlock.
	EnvLease = "SADDLE_RUNQ_LEASE"
	// EnvBypass set to "off" skips the queue entirely (owner escape hatch).
	EnvBypass = "SADDLE_RUNQ"
)

// Priorities. Waiting AgingStep raises a waiter's priority by one, so a
// background run outranks a fresh worker run after 10 steps.
const (
	PrioBackground = 0
	PrioWorker     = 10
	PrioGate       = 20 // the merge train and pre-publish gates
)

// Options configure a Queue. Zero values take the defaults noted.
type Options struct {
	Path         string         // the database file; DefaultPath() when empty
	Slots        map[string]int // slots per class, written when a class is first seen
	DefaultSlots int            // slots for classes not in Slots (1)
	Heartbeat    time.Duration  // how often holders and waiters check in (2s)
	StaleAfter   time.Duration  // a lease without a heartbeat this long is dead (15s)
	PollMin      time.Duration  // first wait between attempts (50ms)
	PollMax      time.Duration  // longest wait between attempts (Heartbeat/2)
	AgingStep    time.Duration  // waiting this long adds one priority (30s)
	Gate         Gate           // optional load gate consulted before a grant
	GateMaxWait  time.Duration  // admit anyway after the gate held a run this long (10m)
	Getenv       func(string) string
	Now          func() time.Time
}

// DefaultPath is the per-user, per-machine queue: $XDG_STATE_HOME/saddle/runq.db.
func DefaultPath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "saddle", "runq.db")
}

// Queue is a handle on the lease store. It is safe for concurrent use.
type Queue struct {
	opts  Options
	db    *sql.DB
	locks string // directory of per-lease lock files
	wake  string // touched on every release so waiters wake early
	host  string

	attempts atomic.Int64 // grant attempts, for tests
}

const schema = `
CREATE TABLE IF NOT EXISTS classes(name TEXT PRIMARY KEY, slots INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS leases(
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  token TEXT NOT NULL UNIQUE,
  class TEXT NOT NULL,
  prio INTEGER NOT NULL,
  state TEXT NOT NULL,
  label TEXT NOT NULL,
  cmd TEXT NOT NULL,
  pid INTEGER NOT NULL,
  host TEXT NOT NULL,
  enqueued INTEGER NOT NULL,
  granted INTEGER NOT NULL DEFAULT 0,
  heartbeat INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS leases_class ON leases(class, state);
CREATE TABLE IF NOT EXISTS history(
  class TEXT NOT NULL,
  label TEXT NOT NULL,
  cmd TEXT NOT NULL,
  waited_ms INTEGER NOT NULL,
  held_ms INTEGER NOT NULL,
  ended INTEGER NOT NULL,
  how TEXT NOT NULL
);
`

// Open opens (creating if needed) the queue. A database file SQLite can't
// read is moved aside and replaced: queue state is ephemeral, and a corrupt
// file must never wedge every heavy run on the machine.
func Open(opts Options) (*Queue, error) {
	opts = withDefaults(opts)
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o755); err != nil {
		return nil, err
	}
	q := &Queue{opts: opts, locks: opts.Path + ".locks", wake: opts.Path + ".wake"}
	q.host, _ = os.Hostname()
	if err := os.MkdirAll(q.locks, 0o755); err != nil {
		return nil, err
	}
	if err := touch(q.wake); err != nil {
		return nil, err
	}
	db, err := openDB(opts.Path)
	if err != nil {
		aside := fmt.Sprintf("%s.corrupt-%d", opts.Path, time.Now().UnixNano())
		if rerr := os.Rename(opts.Path, aside); rerr != nil {
			return nil, fmt.Errorf("runq: %w (and moving it aside failed: %v)", err, rerr)
		}
		for _, sfx := range []string{"-wal", "-shm"} {
			_ = os.Rename(opts.Path+sfx, aside+sfx)
		}
		if db, err = openDB(opts.Path); err != nil {
			return nil, err
		}
	}
	q.db = db
	return q, nil
}

func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection per process: transactions are short, and it keeps this
	// process's goroutines from contending for SQLite's write lock.
	db.SetMaxOpenConns(1)
	var ok string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&ok); err != nil || ok != "ok" {
		db.Close()
		if err == nil {
			err = fmt.Errorf("quick_check: %s", ok)
		}
		return nil, fmt.Errorf("runq: unreadable queue %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("runq: schema: %w", err)
	}
	return db, nil
}

func withDefaults(o Options) Options {
	if o.Path == "" {
		o.Path = DefaultPath()
	}
	if o.DefaultSlots <= 0 {
		o.DefaultSlots = 1
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 2 * time.Second
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = 15 * time.Second
	}
	if o.PollMin <= 0 {
		o.PollMin = 50 * time.Millisecond
	}
	if o.PollMax <= 0 {
		o.PollMax = o.Heartbeat / 2
	}
	if o.PollMax < o.PollMin {
		o.PollMax = o.PollMin
	}
	if o.AgingStep <= 0 {
		o.AgingStep = 30 * time.Second
	}
	if o.GateMaxWait <= 0 {
		o.GateMaxWait = 10 * time.Minute
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Close releases the database handle. Leases still held stay held until
// released or their process exits.
func (q *Queue) Close() error { return q.db.Close() }

// Path is the database file.
func (q *Queue) Path() string { return q.opts.Path }

// Request asks for a slot.
type Request struct {
	Class string
	Prio  int
	Label string // who is asking (task id); SADDLE_TASK or "pid N" when empty
	Cmd   string // what will run, for status
	// OnWait is called while queued, whenever the position or holder changes.
	OnWait func(Wait)
}

// Wait describes a queued request.
type Wait struct {
	Class    string
	Position int // 1 is next in line
	Waiters  int
	Holders  []Entry
	Gated    string // why the load gate holds the run, if it does
	Since    time.Time
}

// String is the one-line status an agent sees.
func (w Wait) String() string {
	s := fmt.Sprintf("queued: position %d of %d for %s", w.Position, w.Waiters, w.Class)
	if len(w.Holders) > 0 {
		var hs []string
		for _, h := range w.Holders {
			hs = append(hs, fmt.Sprintf("%s (%s, %s)", h.Label, h.Cmd, h.Age.Round(time.Second)))
		}
		s += ", holder " + strings.Join(hs, ", ")
	}
	if w.Gated != "" {
		s += ", waiting on load: " + w.Gated
	}
	return s
}

// Lease is a held slot (or a nested or bypassed stand-in for one).
type Lease struct {
	q        *Queue
	token    string
	class    string
	label    string
	cmd      string
	nested   bool
	bypassed bool
	waited   time.Duration
	granted  time.Time
	lock     *os.File
	stop     chan struct{}
	lost     chan struct{}
	done     sync.WaitGroup
	once     sync.Once
}

// Token is passed to children through EnvLease.
func (l *Lease) Token() string { return l.token }

// Nested reports a lease that rode on an outer one from EnvLease.
func (l *Lease) Nested() bool { return l.nested }

// Bypassed reports a lease granted because the queue is off.
func (l *Lease) Bypassed() bool { return l.bypassed }

// Waited is how long the request queued.
func (l *Lease) Waited() time.Duration { return l.waited }

// Lost is closed if the lease was reaped or killed while held.
func (l *Lease) Lost() <-chan struct{} { return l.lost }

// Release frees the slot. It is idempotent.
func (l *Lease) Release() error { return l.end("ok") }

func (l *Lease) end(how string) error {
	var err error
	l.once.Do(func() {
		if l.nested || l.bypassed {
			return
		}
		close(l.stop)
		l.done.Wait()
		q := l.q
		err = q.tx(func(tx *sql.Tx) error {
			res, err := tx.Exec(`DELETE FROM leases WHERE token=?`, l.token)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil // reaped or killed; nothing to record
			}
			return q.record(tx, l.class, l.label, l.cmd, l.waited, q.opts.Now().Sub(l.granted), how)
		})
		_ = os.Remove(l.lock.Name())
		l.lock.Close()
		q.notify()
	})
	return err
}

func (q *Queue) record(tx *sql.Tx, class, label, cmd string, waited, held time.Duration, how string) error {
	_, err := tx.Exec(`INSERT INTO history(class, label, cmd, waited_ms, held_ms, ended, how) VALUES(?,?,?,?,?,?,?)`,
		class, label, cmd, waited.Milliseconds(), held.Milliseconds(), q.opts.Now().UnixMilli(), how)
	return err
}

// Acquire queues for a slot in req.Class and returns once it holds one, ctx
// ends, or the queue fails. It never spins: between attempts it sleeps with
// bounded backoff (PollMin doubling to PollMax) and wakes early when another
// lease is released.
func (q *Queue) Acquire(ctx context.Context, req Request) (*Lease, error) {
	if req.Class == "" {
		return nil, errors.New("runq: empty class")
	}
	if strings.EqualFold(q.opts.Getenv(EnvBypass), "off") {
		return &Lease{q: q, class: req.Class, bypassed: true, lost: make(chan struct{})}, nil
	}
	if req.Label == "" {
		req.Label = q.opts.Getenv("SADDLE_TASK")
	}
	if req.Label == "" {
		req.Label = fmt.Sprintf("pid %d", os.Getpid())
	}
	if outer := q.opts.Getenv(EnvLease); outer != "" {
		ok, err := q.liveRunning(outer)
		if err != nil {
			return nil, err
		}
		if ok {
			return &Lease{q: q, token: outer, class: req.Class, nested: true, lost: make(chan struct{})}, nil
		}
		// A stale token (the outer run ended or died): queue normally.
	}

	l, err := q.enqueue(req)
	if err != nil {
		return nil, err
	}
	enqueued := q.opts.Now()
	wake := q.watchWake()
	if wake != nil {
		defer wake.Close()
	}
	backoff := q.opts.PollMin
	var last string
	var gatedSince time.Time
	for {
		granted, w, err := q.try(l, req, &gatedSince)
		if err != nil {
			_ = l.abandon()
			return nil, err
		}
		if granted {
			l.waited = q.opts.Now().Sub(enqueued)
			l.granted = q.opts.Now()
			l.startHeartbeat()
			return l, nil
		}
		w.Since = enqueued
		if key := waitKey(w); key != last && req.OnWait != nil {
			last = key
			req.OnWait(w)
		}
		timer := time.NewTimer(backoff)
		var events <-chan fsnotify.Event
		var errs <-chan error
		if wake != nil {
			events, errs = wake.Events, wake.Errors
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = l.abandon()
			return nil, ctx.Err()
		case <-events:
			backoff = q.opts.PollMin
		case <-errs:
		case <-timer.C:
			backoff = min(backoff*2, q.opts.PollMax)
		}
		timer.Stop()
	}
}

func waitKey(w Wait) string {
	k := fmt.Sprintf("%d/%d/%s", w.Position, w.Waiters, w.Gated)
	for _, h := range w.Holders {
		k += "/" + h.Token
	}
	return k
}

func (q *Queue) enqueue(req Request) (*Lease, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	// Lock file first, row second: a row whose lock file is missing or
	// unlocked always means its process is gone.
	f, err := lockNew(filepath.Join(q.locks, token))
	if err != nil {
		return nil, err
	}
	now := q.opts.Now().UnixMilli()
	err = q.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO classes(name, slots) VALUES(?, ?)`, req.Class, q.slotsFor(req.Class)); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO leases(token, class, prio, state, label, cmd, pid, host, enqueued, heartbeat)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, token, req.Class, req.Prio, "waiting", req.Label, req.Cmd, os.Getpid(), q.host, now, now)
		return err
	})
	if err != nil {
		_ = os.Remove(f.Name())
		f.Close()
		return nil, err
	}
	return &Lease{q: q, token: token, class: req.Class, label: req.Label, cmd: req.Cmd, lock: f,
		stop: make(chan struct{}), lost: make(chan struct{})}, nil
}

func (q *Queue) slotsFor(class string) int {
	if n, ok := q.opts.Slots[class]; ok && n > 0 {
		return n
	}
	return q.opts.DefaultSlots
}

// abandon drops a waiting lease (ctx canceled or an error).
func (l *Lease) abandon() error {
	var err error
	l.once.Do(func() {
		err = l.q.tx(func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM leases WHERE token=?`, l.token)
			return err
		})
		_ = os.Remove(l.lock.Name())
		l.lock.Close()
		l.q.notify()
	})
	return err
}

// try is one attempt: reap the dead, heartbeat, and promote l if its rank
// fits in the free slots (and the gate admits it).
func (q *Queue) try(l *Lease, req Request, gatedSince *time.Time) (granted bool, w Wait, err error) {
	q.attempts.Add(1)
	err = q.tx(func(tx *sql.Tx) error {
		if err := q.reap(tx); err != nil {
			return err
		}
		now := q.opts.Now()
		res, err := tx.Exec(`UPDATE leases SET heartbeat=? WHERE token=?`, now.UnixMilli(), l.token)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("runq: request %s was removed from the queue (killed)", l.token)
		}
		cs, err := q.classStatus(tx, req.Class, now)
		if err != nil {
			return err
		}
		w = Wait{Class: req.Class, Waiters: len(cs.Waiters), Holders: cs.Holders}
		for _, e := range cs.Waiters {
			if e.Token == l.token {
				w.Position = e.Position
			}
		}
		free := cs.Slots - len(cs.Holders)
		if w.Position == 0 || w.Position > free {
			*gatedSince = time.Time{}
			return nil
		}
		if q.opts.Gate != nil {
			if ok, why := q.opts.Gate.Admit(); !ok {
				if gatedSince.IsZero() {
					*gatedSince = now
				}
				if now.Sub(*gatedSince) < q.opts.GateMaxWait {
					w.Gated = why
					return nil
				}
			}
		}
		if _, err := tx.Exec(`UPDATE leases SET state='running', granted=? WHERE token=?`, now.UnixMilli(), l.token); err != nil {
			return err
		}
		granted = true
		return nil
	})
	return granted, w, err
}

// reap deletes leases whose process is gone: their lock file is free (the
// kernel released it when the process died) or, for leases this host can't
// check, their heartbeat is older than StaleAfter.
func (q *Queue) reap(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT token, host, heartbeat, class, label, cmd, enqueued, granted FROM leases`)
	if err != nil {
		return err
	}
	type row struct {
		token, host, class, label, cmd string
		hb, enq, granted               int64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.token, &r.host, &r.hb, &r.class, &r.label, &r.cmd, &r.enq, &r.granted); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	now := q.opts.Now()
	reaped := false
	live := map[string]bool{}
	for _, r := range all {
		live[r.token] = true
	}
	q.sweepLocks(live)
	for _, r := range all {
		dead := false
		if r.host == q.host {
			dead = q.lockFree(r.token)
		}
		if !dead && now.Sub(time.UnixMilli(r.hb)) > q.opts.StaleAfter {
			dead = true
		}
		if !dead {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM leases WHERE token=?`, r.token); err != nil {
			return err
		}
		if r.granted > 0 {
			waited := time.Duration(r.granted-r.enq) * time.Millisecond
			if err := q.record(tx, r.class, r.label, r.cmd, waited, now.Sub(time.UnixMilli(r.granted)), "reaped"); err != nil {
				return err
			}
		}
		_ = os.Remove(filepath.Join(q.locks, r.token))
		reaped = true
	}
	if reaped {
		q.notify()
	}
	return nil
}

// lockNew creates and locks path. Another process's sweep may probe the new
// file (holding its lock for a moment) and unlink it between our create and
// our flock, so the flock blocks through the probe, and if the locked inode
// is no longer the one at path we start over.
func lockNew(path string) (*os.File, error) {
	for range 3 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return nil, fmt.Errorf("runq: lock %s: %w", path, err)
		}
		held, err1 := f.Stat()
		named, err2 := os.Stat(path)
		if err1 == nil && err2 == nil && os.SameFile(held, named) {
			return f, nil
		}
		f.Close()
	}
	return nil, fmt.Errorf("runq: lock %s: kept disappearing", path)
}

// sweepLocks removes lock files with no lease row whose lock is free: left
// by a process killed between creating its lock file and inserting its row,
// or between deleting its row and removing the file.
func (q *Queue) sweepLocks(live map[string]bool) {
	ents, err := os.ReadDir(q.locks)
	if err != nil {
		return
	}
	for _, e := range ents {
		if live[e.Name()] {
			continue
		}
		path := filepath.Join(q.locks, e.Name())
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			_ = os.Remove(path) // still locked by us, so no one can adopt it mid-remove
		}
		f.Close()
	}
}

// lockFree reports whether nobody holds token's lock file, i.e. its process
// has exited. The probe takes and drops the lock.
func (q *Queue) lockFree(token string) bool {
	f, err := os.OpenFile(filepath.Join(q.locks, token), os.O_RDWR, 0)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return false
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true
}

func (q *Queue) liveRunning(token string) (bool, error) {
	var ok bool
	err := q.tx(func(tx *sql.Tx) error {
		if err := q.reap(tx); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM leases WHERE token=? AND state='running'`, token).Scan(&n); err != nil {
			return err
		}
		ok = n > 0
		return nil
	})
	return ok, err
}

func (l *Lease) startHeartbeat() {
	l.done.Add(1)
	go func() {
		defer l.done.Done()
		t := time.NewTicker(l.q.opts.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
			}
			var n int64
			err := l.q.tx(func(tx *sql.Tx) error {
				res, err := tx.Exec(`UPDATE leases SET heartbeat=? WHERE token=?`, l.q.opts.Now().UnixMilli(), l.token)
				if err == nil {
					n, _ = res.RowsAffected()
				}
				return err
			})
			if err == nil && n == 0 {
				close(l.lost)
				return
			}
		}
	}()
}

// Entry is one lease in Status.
type Entry struct {
	Token    string
	Class    string
	Label    string
	Cmd      string
	PID      int
	Prio     int // effective (aged) priority
	Position int // 1-based among waiters; 0 for holders
	Age      time.Duration
}

// ClassStatus is one class's slots, holders and waiters (in grant order).
type ClassStatus struct {
	Class   string
	Slots   int
	Holders []Entry
	Waiters []Entry
}

// Status reaps dead leases and reports every class.
func (q *Queue) Status() ([]ClassStatus, error) {
	var out []ClassStatus
	err := q.tx(func(tx *sql.Tx) error {
		if err := q.reap(tx); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT name FROM classes ORDER BY name`)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			names = append(names, n)
		}
		rows.Close()
		now := q.opts.Now()
		for _, n := range names {
			cs, err := q.classStatus(tx, n, now)
			if err != nil {
				return err
			}
			out = append(out, cs)
		}
		return nil
	})
	return out, err
}

func (q *Queue) classStatus(tx *sql.Tx, class string, now time.Time) (ClassStatus, error) {
	cs := ClassStatus{Class: class, Slots: q.slotsFor(class)}
	if err := tx.QueryRow(`SELECT slots FROM classes WHERE name=?`, class).Scan(&cs.Slots); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return cs, err
	}
	rows, err := tx.Query(`SELECT seq, token, label, cmd, pid, prio, state, enqueued, granted FROM leases WHERE class=? ORDER BY seq`, class)
	if err != nil {
		return cs, err
	}
	defer rows.Close()
	type waiter struct {
		Entry
		seq int64
	}
	var ws []waiter
	for rows.Next() {
		var e Entry
		var seq, enq, granted int64
		var state string
		if err := rows.Scan(&seq, &e.Token, &e.Label, &e.Cmd, &e.PID, &e.Prio, &state, &enq, &granted); err != nil {
			return cs, err
		}
		e.Class = class
		if state == "running" {
			e.Age = now.Sub(time.UnixMilli(granted))
			cs.Holders = append(cs.Holders, e)
			continue
		}
		e.Age = now.Sub(time.UnixMilli(enq))
		e.Prio += int(e.Age / q.opts.AgingStep)
		ws = append(ws, waiter{e, seq})
	}
	if err := rows.Err(); err != nil {
		return cs, err
	}
	sort.SliceStable(ws, func(i, j int) bool {
		if ws[i].Prio != ws[j].Prio {
			return ws[i].Prio > ws[j].Prio
		}
		return ws[i].seq < ws[j].seq
	})
	for i, w := range ws {
		w.Position = i + 1
		cs.Waiters = append(cs.Waiters, w.Entry)
	}
	return cs, nil
}

// SetSlots changes a class's slot count for every process on the machine.
func (q *Queue) SetSlots(class string, n int) error {
	err := q.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO classes(name, slots) VALUES(?, ?) ON CONFLICT(name) DO UPDATE SET slots=excluded.slots`, class, n)
		return err
	})
	q.notify()
	return err
}

// Kill removes a lease (holder or waiter) by token or token prefix. The
// holder's command keeps running but its Lost channel closes; a waiter's
// Acquire fails.
func (q *Queue) Kill(token string) error {
	var n int64
	err := q.tx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM leases WHERE token LIKE ? || '%'`, token)
		if err == nil {
			n, _ = res.RowsAffected()
		}
		return err
	})
	if err == nil && n == 0 {
		return fmt.Errorf("runq: no lease %q", token)
	}
	q.notify()
	return err
}

// tx runs fn in an immediate (write-locked) transaction.
func (q *Queue) tx(fn func(*sql.Tx) error) error {
	tx, err := q.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// notify wakes waiters in every process.
func (q *Queue) notify() { _ = os.WriteFile(q.wake, []byte{'.'}, 0o644) }

// watchWake watches the wake file; nil if inotify is unavailable, in which
// case waiters fall back to the backoff timer alone.
func (q *Queue) watchWake() *fsnotify.Watcher {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil
	}
	if err := w.Add(q.wake); err != nil {
		w.Close()
		return nil
	}
	return w
}

func touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

func newToken() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
