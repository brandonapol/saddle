// Package store is saddle's shared state: a SQLite database in WAL mode that
// the CLI, Claude Code hooks and MCP servers all open directly.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Task statuses.
const (
	Running  = "running"
	Idle     = "idle"      // agent stopped and is waiting at its prompt
	NeedsYou = "needs_you" // agent asked for permission or input
	Done     = "done"      // queued in the merge train
	Conflict = "conflict"  // returned from the train; agent must sync and resolve
	Landed   = "landed"
	Killed   = "killed"
)

// Train entry states.
const (
	Queued     = "queued"
	TrainOK    = "landed"
	TrainError = "conflict"
	TestFailed = "test_failed"
	// OnHold is a queued entry the owner held back: land skips it, it keeps
	// its place, and done doesn't release it.
	OnHold = "on_hold"
)

const (
	RoleWorker       = "worker"
	RoleOrchestrator = "orchestrator"
)

// Notice kinds. Action notices wake an idle agent; info notices ride along
// on the agent's next tool call.
const (
	NoticeInfo   = "info"
	NoticeAction = "action"
)

var ErrNotFound = errors.New("not found")

type Task struct {
	ID        string
	Title     string
	Prompt    string
	Parent    string
	Role      string
	Model     string
	Branch    string
	Worktree  string
	Window    string
	Status    string
	Summary   string
	SessionID string
	PR        string
	Issue     int // GitHub issue this task works on; its PR closes it
	CreatedAt time.Time
}

// Active reports whether the task still holds claims and can receive notices.
func (t Task) Active() bool { return t.Status != Landed && t.Status != Killed }

type TrainEntry struct {
	Task     string
	Seq      int64
	State    string
	Note     string
	Attempts int
}

type Notice struct {
	ID   int64
	Kind string
	Text string
}

type Rename struct{ Old, New string }

type Store struct{ db *sql.DB }

var migrations = []string{`
CREATE TABLE tasks(
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  prompt TEXT NOT NULL DEFAULT '',
  parent TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'worker',
  model TEXT NOT NULL DEFAULT '',
  branch TEXT NOT NULL DEFAULT '',
  worktree TEXT NOT NULL DEFAULT '',
  window TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  pr TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE claims(task TEXT NOT NULL, glob TEXT NOT NULL, PRIMARY KEY(task, glob));
CREATE TABLE events(id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, task TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL, data TEXT NOT NULL DEFAULT '');
CREATE TABLE train(task TEXT PRIMARY KEY, seq INTEGER NOT NULL, state TEXT NOT NULL, note TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0);
CREATE TABLE notices(id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, task TEXT NOT NULL, kind TEXT NOT NULL, text TEXT NOT NULL, delivered INTEGER NOT NULL DEFAULT 0);
CREATE TABLE renames(id INTEGER PRIMARY KEY, by_task TEXT NOT NULL, old TEXT NOT NULL, new TEXT NOT NULL);
`, `
ALTER TABLE tasks ADD COLUMN issue INTEGER NOT NULL DEFAULT 0;
CREATE TABLE chat(id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, role TEXT NOT NULL, text TEXT NOT NULL);
`, `
CREATE TABLE usage(
  minute INTEGER NOT NULL,
  task TEXT NOT NULL,
  model TEXT NOT NULL,
  session TEXT NOT NULL,
  input INTEGER NOT NULL DEFAULT 0,
  output INTEGER NOT NULL DEFAULT 0,
  cache_read INTEGER NOT NULL DEFAULT 0,
  cache_creation INTEGER NOT NULL DEFAULT 0,
  messages INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(minute, task, model, session)
);
CREATE INDEX usage_session ON usage(session);
`, `
CREATE TABLE narrator_spend(day TEXT PRIMARY KEY, usd REAL NOT NULL DEFAULT 0);
`}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	return s.tx(func(tx *sql.Tx) error {
		var v int
		if err := tx.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
			return err
		}
		// A newer binary already migrated this database. Leave user_version
		// alone: lowering it would make that binary re-run its migrations.
		if v >= len(migrations) {
			return nil
		}
		for i := v; i < len(migrations); i++ {
			if _, err := tx.Exec(migrations[i]); err != nil {
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		_, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, len(migrations)))
		return err
	})
}

// tx runs fn in an immediate (write-locked) transaction.
func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func now() int64 { return time.Now().Unix() }

const taskCols = `id, title, prompt, parent, role, model, branch, worktree, window, status, summary, session_id, pr, issue, created_at`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var created int64
	err := row.Scan(&t.ID, &t.Title, &t.Prompt, &t.Parent, &t.Role, &t.Model, &t.Branch, &t.Worktree,
		&t.Window, &t.Status, &t.Summary, &t.SessionID, &t.PR, &t.Issue, &created)
	t.CreatedAt = time.Unix(created, 0)
	return t, err
}

func (s *Store) CreateTask(t Task) error {
	_, err := s.db.Exec(`INSERT INTO tasks(`+taskCols+`, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Title, t.Prompt, t.Parent, t.Role, t.Model, t.Branch, t.Worktree, t.Window, t.Status,
		t.Summary, t.SessionID, t.PR, t.Issue, now(), now())
	return err
}

func (s *Store) Task(id string) (Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("task %s: %w", id, ErrNotFound)
	}
	return t, err
}

func (s *Store) Tasks() ([]Task, error) {
	rows, err := s.db.Query(`SELECT ` + taskCols + ` FROM tasks ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetField updates one column of a task. Only known column names are accepted.
func (s *Store) SetField(id, field, value string) error {
	switch field {
	case "status", "window", "summary", "session_id", "pr", "worktree", "model":
	default:
		return fmt.Errorf("unknown task field %q", field)
	}
	_, err := s.db.Exec(`UPDATE tasks SET `+field+` = ?, updated_at = ? WHERE id = ?`, value, now(), id)
	return err
}

func (s *Store) SetStatus(id, status string) error { return s.SetField(id, "status", status) }

// NextID returns the next free id of the form t<N>.
func (s *Store) NextID() (string, error) {
	var n int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(CAST(SUBSTR(id, 2) AS INTEGER)), 0) FROM tasks WHERE id GLOB 't[0-9]*'`).Scan(&n)
	return fmt.Sprintf("t%d", n+1), err
}

// Claims returns every claim held by an active task, keyed by task id.
func (s *Store) Claims() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT c.task, c.glob FROM claims c JOIN tasks t ON t.id = c.task
		WHERE t.status NOT IN (?, ?) ORDER BY c.task, c.glob`, Landed, Killed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var task, glob string
		if err := rows.Scan(&task, &glob); err != nil {
			return nil, err
		}
		out[task] = append(out[task], glob)
	}
	return out, rows.Err()
}

// ClaimFunc decides, given every active claim, which of the requested globs
// the task may take. It runs inside the write transaction so two agents can't
// both win the same path.
type ClaimFunc func(all map[string][]string) (grant []string, err error)

func (s *Store) Claim(task string, decide ClaimFunc) ([]string, error) {
	var granted []string
	err := s.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT c.task, c.glob FROM claims c JOIN tasks t ON t.id = c.task
			WHERE t.status NOT IN (?, ?)`, Landed, Killed)
		if err != nil {
			return err
		}
		all := map[string][]string{}
		for rows.Next() {
			var t, g string
			if err := rows.Scan(&t, &g); err != nil {
				rows.Close()
				return err
			}
			all[t] = append(all[t], g)
		}
		rows.Close()
		granted, err = decide(all)
		if err != nil {
			return err
		}
		for _, g := range granted {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO claims(task, glob) VALUES(?, ?)`, task, g); err != nil {
				return err
			}
		}
		return nil
	})
	return granted, err
}

func (s *Store) Release(task string, globs ...string) error {
	if len(globs) == 0 {
		_, err := s.db.Exec(`DELETE FROM claims WHERE task = ?`, task)
		return err
	}
	for _, g := range globs {
		if _, err := s.db.Exec(`DELETE FROM claims WHERE task = ? AND glob = ?`, task, g); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceClaim swaps one glob for another (used when the rename map moves a claim).
func (s *Store) ReplaceClaim(task, old, new string) error {
	return s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM claims WHERE task = ? AND glob = ?`, task, old); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT OR IGNORE INTO claims(task, glob) VALUES(?, ?)`, task, new)
		return err
	})
}

// Event appends to the activity log. It is best-effort: a failed log write
// must never block an agent or the train.
func (s *Store) Event(task, kind, data string) {
	_, _ = s.db.Exec(`INSERT INTO events(ts, task, kind, data) VALUES(?,?,?,?)`, now(), task, kind, data)
}

type Event struct {
	ID   int64 // increases with every event; a cursor for EventsSince
	TS   time.Time
	Task string
	Kind string
	Data string
}

const eventCols = `id, ts, task, kind, data`

func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Task, &e.Kind, &e.Data); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Events returns the newest n events, oldest first.
func (s *Store) Events(n int) ([]Event, error) {
	rows, err := s.db.Query(`SELECT `+eventCols+` FROM (SELECT * FROM events ORDER BY id DESC LIMIT ?) ORDER BY id`, n)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// EventsSince returns every event with an id above after, oldest first. Pass
// the last ID seen to resume; LastEventID gives a cursor that skips history.
func (s *Store) EventsSince(after int64) ([]Event, error) {
	rows, err := s.db.Query(`SELECT `+eventCols+` FROM events WHERE id > ? ORDER BY id`, after)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// LastEventID is the id of the newest event, or 0 when there are none.
func (s *Store) LastEventID() (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&id)
	return id, err
}

// LastEventTimes maps each task to the time of its newest event: its
// heartbeat, since hooks log every tool call.
func (s *Store) LastEventTimes() (map[string]time.Time, error) {
	rows, err := s.db.Query(`SELECT task, MAX(ts) FROM events WHERE task != '' GROUP BY task`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var task string
		var ts int64
		if err := rows.Scan(&task, &ts); err != nil {
			return nil, err
		}
		out[task] = time.Unix(ts, 0)
	}
	return out, rows.Err()
}

// NarratorSpend is the narrator's recorded API spend in USD for day
// (YYYY-MM-DD), or 0 if none.
func (s *Store) NarratorSpend(day string) (float64, error) {
	var usd float64
	err := s.db.QueryRow(`SELECT usd FROM narrator_spend WHERE day = ?`, day).Scan(&usd)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return usd, err
}

// SetNarratorSpend records the narrator's total spend for day, so a restart
// does not reset its daily cap.
func (s *Store) SetNarratorSpend(day string, usd float64) error {
	_, err := s.db.Exec(`INSERT INTO narrator_spend(day, usd) VALUES(?, ?)
		ON CONFLICT(day) DO UPDATE SET usd = excluded.usd`, day, usd)
	return err
}

// Enqueue puts a task at the back of the merge train (or back in line after a conflict).
func (s *Store) Enqueue(task string) error {
	return s.tx(func(tx *sql.Tx) error {
		var seq int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM train`).Scan(&seq); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO train(task, seq, state) VALUES(?, ?, ?)
			ON CONFLICT(task) DO UPDATE SET seq = excluded.seq, state = excluded.state, note = ''
			WHERE train.state <> ?`, task, seq, Queued, OnHold)
		return err
	})
}

// SetTrainOrder puts the named entries in the given order, reusing the seqs
// they already hold, so entries not named keep their places.
func (s *Store) SetTrainOrder(tasks []string) error {
	return s.tx(func(tx *sql.Tx) error {
		var seqs []int64
		for _, t := range tasks {
			var seq int64
			if err := tx.QueryRow(`SELECT seq FROM train WHERE task = ?`, t).Scan(&seq); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%s is not in the train: %w", t, ErrNotFound)
				}
				return err
			}
			seqs = append(seqs, seq)
		}
		slices.Sort(seqs)
		for i, t := range tasks {
			if _, err := tx.Exec(`UPDATE train SET seq = ? WHERE task = ?`, seqs[i], t); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) Train() ([]TrainEntry, error) {
	rows, err := s.db.Query(`SELECT task, seq, state, note, attempts FROM train ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrainEntry
	for rows.Next() {
		var e TrainEntry
		if err := rows.Scan(&e.Task, &e.Seq, &e.State, &e.Note, &e.Attempts); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) SetTrain(task, state, note string, failed bool) error {
	inc := 0
	if failed {
		inc = 1
	}
	_, err := s.db.Exec(`UPDATE train SET state = ?, note = ?, attempts = attempts + ? WHERE task = ?`, state, note, inc, task)
	return err
}

func (s *Store) Notify(task, kind, text string) error {
	_, err := s.db.Exec(`INSERT INTO notices(ts, task, kind, text) VALUES(?,?,?,?)`, now(), task, kind, text)
	return err
}

// TakeNotices returns undelivered notices for a task and marks them delivered.
// With actionOnly, info notices are left for later.
func (s *Store) TakeNotices(task string, actionOnly bool) ([]Notice, error) {
	var out []Notice
	err := s.tx(func(tx *sql.Tx) error {
		q := `SELECT id, kind, text FROM notices WHERE task = ? AND delivered = 0`
		args := []any{task}
		if actionOnly {
			q += ` AND kind = ?`
			args = append(args, NoticeAction)
		}
		rows, err := tx.Query(q+` ORDER BY id`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n Notice
			if err := rows.Scan(&n.ID, &n.Kind, &n.Text); err != nil {
				rows.Close()
				return err
			}
			out = append(out, n)
		}
		rows.Close()
		for _, n := range out {
			if _, err := tx.Exec(`UPDATE notices SET delivered = 1 WHERE id = ?`, n.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// PeekNotices returns undelivered notices for a task without marking them.
// Pair it with MarkDelivered once they were seen.
func (s *Store) PeekNotices(task string, actionOnly bool) ([]Notice, error) {
	q := `SELECT id, kind, text FROM notices WHERE task = ? AND delivered = 0`
	args := []any{task}
	if actionOnly {
		q += ` AND kind = ?`
		args = append(args, NoticeAction)
	}
	rows, err := s.db.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notice
	for rows.Next() {
		var n Notice
		if err := rows.Scan(&n.ID, &n.Kind, &n.Text); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkDelivered marks the given notices delivered.
func (s *Store) MarkDelivered(ns []Notice) error {
	return s.tx(func(tx *sql.Tx) error {
		for _, n := range ns {
			if _, err := tx.Exec(`UPDATE notices SET delivered = 1 WHERE id = ?`, n.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

// OldestPendingAction returns when a task's oldest undelivered action notice
// was queued; ok is false when it has none.
func (s *Store) OldestPendingAction(task string) (ts time.Time, ok bool, err error) {
	var sec sql.NullInt64
	err = s.db.QueryRow(`SELECT MIN(ts) FROM notices WHERE task = ? AND delivered = 0 AND kind = ?`, task, NoticeAction).Scan(&sec)
	if err != nil || !sec.Valid {
		return time.Time{}, false, err
	}
	return time.Unix(sec.Int64, 0), true, nil
}

func (s *Store) PendingNotices(task string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM notices WHERE task = ? AND delivered = 0`, task).Scan(&n)
	return n, err
}

// PendingActionNotices counts a task's undelivered action notices.
func (s *Store) PendingActionNotices(task string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM notices WHERE task = ? AND delivered = 0 AND kind = ?`, task, NoticeAction).Scan(&n)
	return n, err
}

// Tables lists the tables RowCounts reports, in schema order.
var Tables = []string{"tasks", "claims", "events", "train", "notices", "renames", "chat", "usage", "narrator_spend"}

// RowCounts returns how many rows each table in Tables holds.
func (s *Store) RowCounts() (map[string]int, error) {
	out := make(map[string]int, len(Tables))
	for _, t := range Tables {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}

// PendingNoticeCounts returns how many undelivered notices each task has.
// Tasks with none are absent.
func (s *Store) PendingNoticeCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT task, COUNT(*) FROM notices WHERE delivered = 0 GROUP BY task`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var task string
		var n int
		if err := rows.Scan(&task, &n); err != nil {
			return nil, err
		}
		out[task] = n
	}
	return out, rows.Err()
}

func (s *Store) AddRenames(byTask string, rs []Rename) error {
	return s.tx(func(tx *sql.Tx) error {
		for _, r := range rs {
			if _, err := tx.Exec(`INSERT INTO renames(by_task, old, new) VALUES(?,?,?)`, byTask, r.Old, r.New); err != nil {
				return err
			}
		}
		return nil
	})
}

// FormatNotices renders notices as one block for injecting into an agent.
func FormatNotices(ns []Notice) string {
	var b strings.Builder
	for i, n := range ns {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("[saddle] ")
		b.WriteString(n.Text)
	}
	return b.String()
}

// Chat roles.
const (
	ChatUser      = "user"
	ChatAssistant = "assistant"
	ChatTool      = "tool"
	ChatEvent     = "event"
	ChatNarrator  = "narrator" // a line from the event narrator
)

type ChatLine struct {
	TS   time.Time
	Role string
	Text string
}

// AddChat appends to the orchestrator conversation shown in the TUI.
func (s *Store) AddChat(role, text string) error {
	_, err := s.db.Exec(`INSERT INTO chat(ts, role, text) VALUES(?,?,?)`, now(), role, text)
	return err
}

// Chat returns the newest n chat lines, oldest first.
func (s *Store) Chat(n int) ([]ChatLine, error) {
	rows, err := s.db.Query(`SELECT ts, role, text FROM (SELECT * FROM chat ORDER BY id DESC LIMIT ?) ORDER BY id`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatLine
	for rows.Next() {
		var c ChatLine
		var ts int64
		if err := rows.Scan(&ts, &c.Role, &c.Text); err != nil {
			return nil, err
		}
		c.TS = time.Unix(ts, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}
