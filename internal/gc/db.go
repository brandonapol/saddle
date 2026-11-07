// Package gc holds the retention rules saddle's automatic garbage collection
// applies (#802): which rows of state.db expire, how many integration backup
// refs stay, and git's own object maintenance. internal/app decides when it
// runs and what is live; this package only removes what the rules say is old.
package gc

import (
	"database/sql"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// NoisyKinds are event kinds written often and read only while fresh. They
// expire after Retention.Events; every other kind (landed, gc, spawn, ...) is
// history and stays. pr_layout stays too, though #802 listed it: the shadow
// reconciler's replay rebuilds which PRs are open from it.
var NoisyKinds = []string{"ci_error", "tool", "ref"}

// Retention says how long rows live. Zero values pick the defaults.
type Retention struct {
	Events   time.Duration // NoisyKinds rows older than this go (7 days)
	Notices  time.Duration // delivered notices older than this go (7 days)
	Chat     time.Duration // chat rows older than this go... (30 days)
	ChatKeep int           // ...except the newest ChatKeep (1000)
}

func (r Retention) withDefaults() Retention {
	if r.Events <= 0 {
		r.Events = 7 * 24 * time.Hour
	}
	if r.Notices <= 0 {
		r.Notices = 7 * 24 * time.Hour
	}
	if r.Chat <= 0 {
		r.Chat = 30 * 24 * time.Hour
	}
	if r.ChatKeep <= 0 {
		r.ChatKeep = 1000
	}
	return r
}

// DBResult counts the rows PruneDB deleted.
type DBResult struct {
	Events, Notices, Chat int64
}

// open connects to a state.db the way the store does, so a prune waits on
// the store's writers instead of failing with SQLITE_BUSY.
func open(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_txlock=immediate")
}

// PruneDB deletes the rows of the state.db at path that r says expired by
// now. It opens its own connection: the store's is not exported, and SQLite
// in WAL mode serialises the writers.
func PruneDB(path string, now time.Time, r Retention) (DBResult, error) {
	r = r.withDefaults()
	var res DBResult
	db, err := open(path)
	if err != nil {
		return res, err
	}
	defer func() { _ = db.Close() }()
	args := []any{now.Add(-r.Events).Unix()}
	for _, k := range NoisyKinds {
		args = append(args, k)
	}
	steps := []struct {
		n    *int64
		q    string
		args []any
	}{
		{&res.Events, `DELETE FROM events WHERE ts < ? AND kind IN (?` + strings.Repeat(`,?`, len(NoisyKinds)-1) + `)`, args},
		{&res.Notices, `DELETE FROM notices WHERE delivered = 1 AND ts < ?`, []any{now.Add(-r.Notices).Unix()}},
		{&res.Chat, `DELETE FROM chat WHERE ts < ? AND id NOT IN (SELECT id FROM chat ORDER BY id DESC LIMIT ?)`, []any{now.Add(-r.Chat).Unix(), r.ChatKeep}},
	}
	for _, s := range steps {
		out, err := db.Exec(s.q, s.args...)
		if err != nil {
			return res, err
		}
		*s.n, _ = out.RowsAffected()
	}
	return res, nil
}
