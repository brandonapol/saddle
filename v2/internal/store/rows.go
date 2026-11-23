package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNotFound is returned when a row asked for by id does not exist.
var ErrNotFound = errors.New("not found")

// Task is one row of tasks. State and Reason are written only with the
// output of core.Transition.
type Task struct {
	ID          int64
	Ticket      string
	Title       string
	Prompt      string
	Role        string
	State       string
	Reason      string
	Since       int64
	Provider    string
	Model       string
	Session     string
	Branch      string
	Worktree    string
	PR          int64
	Touches     []string
	After       []int64
	FixAttempts int
	Held        bool
	Created     int64
}

// Name is how a task is shown: t14.
func (t Task) Name() string { return fmt.Sprintf("t%d", t.ID) }

// Event is one row of the audit log. Nothing reads events to learn state.
type Event struct {
	ID   int64
	TS   int64
	Task int64
	Kind string
	Data string
}

// Attempt is saddle's record of one turn.
type Attempt struct {
	ID         int64
	Task       int64
	Provider   string
	Model      string
	Argv       []string
	ResumeArgv []string
	PID        int
	Started    int64
	Ended      int64
	Outcome    string
	Detail     string
	TokensIn   int64
	TokensOut  int64
	Cost       float64
}

// Message is one inbox row.
type Message struct {
	ID      int64
	To      int64
	Sender  string
	Kind    string
	Dedup   string // a second message to the same task with the same non-empty key is dropped
	Body    string
	Created int64
}

// Question is something an agent asked.
type Question struct {
	ID         int64
	Task       int64
	Body       string
	Options    []string
	Asked      int64
	Answered   bool
	Answer     string
	AnsweredBy string
}

// Setting is one key in settings.
type Setting struct {
	Key, Value string
	Updated    int64
	UpdatedBy  string
}

// Provider is one agent CLI's availability.
type Provider struct {
	Name      string
	Available bool
	OutUntil  int64
	Detail    string
}

// Retry is a failed action waiting to run again.
type Retry struct {
	Key        string
	InputsHash string
	Attempts   int
	NextAt     int64
	LastError  string
}

type scanner interface{ Scan(...any) error }

func toJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return "[]"
	}
	return string(b)
}

func fromJSON[T any](s string, what string) ([]T, error) {
	var out []T
	if s == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("%s holds %q, not a JSON array: %w", what, s, err)
	}
	return out, nil
}

const taskCols = `id, ticket, title, prompt, role, state, reason, since, provider, model,
  session, branch, worktree, pr, touches, after, fix_attempts, held, created`

func scanTask(r scanner) (Task, error) {
	var t Task
	var touches, after string
	err := r.Scan(&t.ID, &t.Ticket, &t.Title, &t.Prompt, &t.Role, &t.State, &t.Reason, &t.Since,
		&t.Provider, &t.Model, &t.Session, &t.Branch, &t.Worktree, &t.PR, &touches, &after,
		&t.FixAttempts, &t.Held, &t.Created)
	if err != nil {
		return t, err
	}
	if t.Touches, err = fromJSON[string](touches, "tasks.touches"); err != nil {
		return t, err
	}
	t.After, err = fromJSON[int64](after, "tasks.after")
	return t, err
}

func scanTasks(rows *sql.Rows) ([]Task, error) {
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

// AddEvent appends to the audit log.
func (t *Tx) AddEvent(task int64, kind, data string) error {
	_, err := t.tx.Exec("INSERT INTO events(ts, task, kind, data) VALUES(?,?,?,?)", t.now, task, kind, data)
	return err
}

// CreateTask inserts a task and its created event, and returns it with its
// id, Since and Created set.
func (t *Tx) CreateTask(task Task) (Task, error) {
	if task.Title == "" || task.State == "" {
		return task, errors.New("a task needs a title and a state")
	}
	if task.Role == "" {
		task.Role = "worker"
	}
	task.Since, task.Created = t.now, t.now
	res, err := t.tx.Exec(`INSERT INTO tasks(ticket, title, prompt, role, state, reason, since, provider, model,
		session, branch, worktree, pr, touches, after, fix_attempts, held, created)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		task.Ticket, task.Title, task.Prompt, task.Role, task.State, task.Reason, task.Since, task.Provider,
		task.Model, task.Session, task.Branch, task.Worktree, task.PR, toJSON(task.Touches), toJSON(task.After),
		task.FixAttempts, task.Held, task.Created)
	if err != nil {
		return task, err
	}
	if task.ID, err = res.LastInsertId(); err != nil {
		return task, err
	}
	return task, t.AddEvent(task.ID, "created", task.State)
}

// Task reads one task.
func (t *Tx) Task(id int64) (Task, error) {
	task, err := scanTask(t.tx.QueryRow("SELECT "+taskCols+" FROM tasks WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return task, fmt.Errorf("task t%d: %w", id, ErrNotFound)
	}
	return task, err
}

// PutTask writes every changeable field of a task and appends the event
// that explains the change, together. Since moves only when the state or
// reason changed.
func (t *Tx) PutTask(task Task, kind, data string) error {
	old, err := t.Task(task.ID)
	if err != nil {
		return err
	}
	since := old.Since
	if old.State != task.State || old.Reason != task.Reason {
		since = t.now
	}
	_, err = t.tx.Exec(`UPDATE tasks SET title=?, prompt=?, state=?, reason=?, since=?, provider=?, model=?,
		session=?, branch=?, worktree=?, pr=?, touches=?, after=?, fix_attempts=?, held=? WHERE id=?`,
		task.Title, task.Prompt, task.State, task.Reason, since, task.Provider, task.Model, task.Session,
		task.Branch, task.Worktree, task.PR, toJSON(task.Touches), toJSON(task.After), task.FixAttempts,
		task.Held, task.ID)
	if err != nil {
		return err
	}
	return t.AddEvent(task.ID, kind, data)
}

// StartAttempt records a turn about to run and returns its id.
func (t *Tx) StartAttempt(a Attempt) (int64, error) {
	res, err := t.tx.Exec(`INSERT INTO attempts(task, provider, model, argv, resume_argv, pid, started)
		VALUES(?,?,?,?,?,?,?)`, a.Task, a.Provider, a.Model, toJSON(a.Argv), toJSON(a.ResumeArgv), a.PID, t.now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// EndAttempt records how a turn ended. An attempt ends once: a second call
// changes nothing and reports false.
func (t *Tx) EndAttempt(id int64, outcome, detail string) (bool, error) {
	res, err := t.tx.Exec("UPDATE attempts SET ended=?, outcome=?, detail=? WHERE id=? AND ended=0",
		t.now, outcome, detail, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Send puts a message in a task's inbox. It reports false when a message
// with the same Dedup key was already sent to that task.
func (t *Tx) Send(m Message) (bool, error) {
	if m.Kind == "" {
		m.Kind = "message"
	}
	res, err := t.tx.Exec(`INSERT INTO inbox(to_task, sender, kind, dedup, body, created) VALUES(?,?,?,?,?,?)
		ON CONFLICT DO NOTHING`, m.To, m.Sender, m.Kind, m.Dedup, m.Body, t.now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

const pendingQuery = `SELECT id, to_task, sender, kind, dedup, body, created FROM inbox
	WHERE to_task = ? AND delivered_attempt IS NULL ORDER BY id`

// Pending lists a task's undelivered messages, oldest first.
func (t *Tx) Pending(task int64) ([]Message, error) {
	rows, err := t.tx.Query(pendingQuery, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.To, &m.Sender, &m.Kind, &m.Dedup, &m.Body, &m.Created); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Deliver marks a task's undelivered messages, up to and including id
// through, as delivered by attempt.
func (t *Tx) Deliver(task, through, attempt int64) error {
	_, err := t.tx.Exec(`UPDATE inbox SET delivered_attempt = ?
		WHERE to_task = ? AND delivered_attempt IS NULL AND id <= ?`, attempt, task, through)
	return err
}

// Ask records a question and returns its id.
func (t *Tx) Ask(task int64, body string, options []string) (int64, error) {
	res, err := t.tx.Exec("INSERT INTO questions(task, body, options, asked) VALUES(?,?,?,?)",
		task, body, toJSON(options), t.now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Answer answers an open question. It reports false when the question was
// already answered, and ErrNotFound when it doesn't exist.
func (t *Tx) Answer(id int64, answer, by string) (bool, error) {
	res, err := t.tx.Exec("UPDATE questions SET answer=?, answered_by=?, answered=? WHERE id=? AND answer IS NULL",
		answer, by, t.now, id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	var one int
	if err := t.tx.QueryRow("SELECT 1 FROM questions WHERE id=?", id).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("question %d: %w", id, ErrNotFound)
	} else if err != nil {
		return false, err
	}
	return false, nil
}

// Setting reads one setting; ok is false when it is unset.
func (t *Tx) Setting(key string) (s Setting, ok bool, err error) {
	err = t.tx.QueryRow("SELECT key, value, updated, updated_by FROM settings WHERE key=?", key).
		Scan(&s.Key, &s.Value, &s.Updated, &s.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	return s, err == nil, err
}

// Set writes a setting and an event saying who changed it.
func (t *Tx) Set(key, value, by string) error {
	_, err := t.tx.Exec(`INSERT INTO settings(key, value, updated, updated_by) VALUES(?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated=excluded.updated, updated_by=excluded.updated_by`,
		key, value, t.now, by)
	if err != nil {
		return err
	}
	return t.AddEvent(0, "setting", key+"="+value+" by "+by)
}

// PutProvider records a provider's availability.
func (t *Tx) PutProvider(p Provider) error {
	_, err := t.tx.Exec(`INSERT INTO providers(name, available, out_until, detail) VALUES(?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET available=excluded.available, out_until=excluded.out_until, detail=excluded.detail`,
		p.Name, p.Available, p.OutUntil, p.Detail)
	return err
}

// PutRetry records a failed action. A row whose inputs changed starts again
// from attempt 1.
func (t *Tx) PutRetry(r Retry) error {
	_, err := t.tx.Exec(`INSERT INTO retries(key, inputs_hash, attempts, next_at, last_error) VALUES(?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET inputs_hash=excluded.inputs_hash, attempts=excluded.attempts,
		next_at=excluded.next_at, last_error=excluded.last_error`,
		r.Key, r.InputsHash, r.Attempts, r.NextAt, r.LastError)
	return err
}

// Retry reads one retry row; ok is false when the action has none.
func (t *Tx) Retry(key string) (r Retry, ok bool, err error) {
	err = t.tx.QueryRow("SELECT key, inputs_hash, attempts, next_at, last_error FROM retries WHERE key=?", key).
		Scan(&r.Key, &r.InputsHash, &r.Attempts, &r.NextAt, &r.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// ClearRetry forgets a retry once its action succeeded.
func (t *Tx) ClearRetry(key string) error {
	_, err := t.tx.Exec("DELETE FROM retries WHERE key=?", key)
	return err
}
