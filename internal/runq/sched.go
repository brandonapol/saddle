package runq

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// ScopeSystemd is Options.Scope (and [runq] scope) for running heavy
// children in a transient user systemd scope with a low CPUWeight.
const ScopeSystemd = "systemd"

// Adaptive slots (docs/runq.md Q3): recomputed at most once per adaptEvery
// from the last adaptWindow of history, for classes with at least
// adaptMinRuns measured runs.
const (
	adaptEvery   = 24 * time.Hour
	adaptWindow  = 7 * 24 * time.Hour
	adaptMinRuns = 5
)

// shape rewrites cmd to start under the scope, nice and ionice the options
// ask for: systemd-run --user --scope … -- nice -n N ionice -t -c3 cmd….
// Each wrapper execs the next, so the child keeps its PID (and Pdeathsig).
// A wrapper whose tool is missing is skipped; the run never fails for it.
// The returned func puts cmd's Path and Args back once it has started, so
// callers still describe the command they asked for.
func (q *Queue) shape(cmd *exec.Cmd, out io.Writer) (restore func()) {
	restore = func() {}
	if cmd.Err != nil || cmd.Path == "" {
		return restore // Start reports it
	}
	var pre []string
	if q.opts.Scope == ScopeSystemd {
		if bin, ok := q.userSystemd(); ok {
			pre = append(pre, bin, "--user", "--scope", "--quiet", "--collect",
				"-p", fmt.Sprintf("CPUWeight=%d", q.opts.CPUWeight))
			if q.opts.CPUQuota != "" {
				pre = append(pre, "-p", "CPUQuota="+q.opts.CPUQuota)
			}
			pre = append(pre, "--")
		} else {
			fmt.Fprintln(out, `runq: scope = "systemd" but no user systemd is reachable; running without a scope`)
		}
	}
	if q.opts.Nice > 0 {
		if bin, err := q.opts.LookPath("nice"); err == nil {
			pre = append(pre, bin, "-n", strconv.Itoa(q.opts.Nice))
		}
	}
	if q.opts.IOIdle && runtime.GOOS == "linux" {
		if bin, err := q.opts.LookPath("ionice"); err == nil {
			pre = append(pre, bin, "-t", "-c3") // -t: run anyway if the class can't be set
		}
	}
	if len(pre) == 0 {
		return restore
	}
	path, args := cmd.Path, cmd.Args
	cmd.Path = pre[0]
	cmd.Args = append(append(pre, path), args[1:]...)
	return func() { cmd.Path, cmd.Args = path, args }
}

// userSystemd finds systemd-run and a reachable user manager.
func (q *Queue) userSystemd() (string, bool) {
	bin, err := q.opts.LookPath("systemd-run")
	if err != nil {
		return "", false
	}
	rt := q.opts.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(rt, "systemd", "private")); err != nil {
		return "", false
	}
	return bin, true
}

// Adapt sizes each class's slots from history when adaptive slots are on
// (Options.TargetUtil > 0) and the last sizing is a day old:
//
//	slots = max(1, floor(cores × target_util / avg_cores(class)))
//
// capped at cores, where avg_cores is the class's CPU time over its run
// time across the last week. Classes with fewer than five measured runs,
// and drained classes, keep their slots. It returns the classes it changed.
func (q *Queue) Adapt() (map[string]int, error) {
	changed := map[string]int{}
	if q.opts.TargetUtil <= 0 {
		return changed, nil
	}
	err := q.tx(func(tx *sql.Tx) error {
		now := q.opts.Now()
		var last int64
		switch err := tx.QueryRow(`SELECT value FROM meta WHERE key='adapted'`).Scan(&last); {
		case err == nil:
			if now.Sub(time.UnixMilli(last)) < adaptEvery {
				return nil
			}
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		rows, err := tx.Query(`SELECT class, COUNT(*), SUM(cpu_ms), SUM(held_ms) FROM history
			WHERE how='ok' AND cpu_ms>0 AND held_ms>0 AND ended>=? GROUP BY class`, now.Add(-adaptWindow).UnixMilli())
		if err != nil {
			return err
		}
		want := map[string]int{}
		for rows.Next() {
			var class string
			var n, cpu, held int64
			if err := rows.Scan(&class, &n, &cpu, &held); err != nil {
				rows.Close()
				return err
			}
			if n < adaptMinRuns {
				continue
			}
			avg := float64(cpu) / float64(held)
			want[class] = min(q.opts.CPUs, max(1, int(math.Floor(float64(q.opts.CPUs)*q.opts.TargetUtil/avg))))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for class, n := range want {
			res, err := tx.Exec(`UPDATE classes SET slots=? WHERE name=? AND slots>0 AND slots<>?`, n, class, n)
			if err != nil {
				return err
			}
			if k, _ := res.RowsAffected(); k > 0 {
				changed[class] = n
			}
		}
		_, err = tx.Exec(`INSERT INTO meta(key, value) VALUES('adapted', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, now.UnixMilli())
		return err
	})
	if len(changed) > 0 {
		q.notify()
	}
	return changed, err
}
