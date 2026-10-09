package runq

import (
	"database/sql"
	"math"
	"sort"
	"time"
)

// HistoryKeep is how long history rows are kept; Open prunes older ones.
const HistoryKeep = 30 * 24 * time.Hour

// ClassStats summarises one class's finished runs (saddle runq stats).
type ClassStats struct {
	Class       string  `json:"class"`
	Runs        int     `json:"runs"`
	HeldP50MS   int64   `json:"held_p50_ms"`
	HeldP95MS   int64   `json:"held_p95_ms"`
	WaitedP50MS int64   `json:"waited_p50_ms"`
	WaitedP95MS int64   `json:"waited_p95_ms"`
	AvgCores    float64 `json:"avg_cores"`      // cpu_ms / held_ms over runs with a CPU reading
	RSSP95KB    int64   `json:"max_rss_p95_kb"` // p95 of max_rss_kb over runs with a reading
	// Overlap is the share of the class's held time during which another
	// run, of any class, was also held (0..1).
	Overlap float64 `json:"overlap"`
}

type statRow struct {
	class                  string
	waited, held, cpu, rss int64
	start, end             int64
}

// Stats reports per-class statistics over runs that ended within since of
// now, ordered by class. A run held from ended-held_ms to ended.
func (q *Queue) Stats(since time.Duration) ([]ClassStats, error) {
	cutoff := q.opts.Now().Add(-since).UnixMilli()
	rows, err := q.db.Query(`SELECT class, waited_ms, held_ms, cpu_ms, max_rss_kb, ended FROM history WHERE ended>=? ORDER BY class, ended`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []statRow
	for rows.Next() {
		var r statRow
		if err := rows.Scan(&r.class, &r.waited, &r.held, &r.cpu, &r.rss, &r.end); err != nil {
			return nil, err
		}
		r.start = r.end - r.held
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return computeStats(all), nil
}

func computeStats(all []statRow) []ClassStats {
	by := map[string][]statRow{}
	for _, r := range all {
		by[r.class] = append(by[r.class], r)
	}
	var out []ClassStats
	for class, rs := range by {
		s := ClassStats{Class: class, Runs: len(rs)}
		var held, waited, rss []int64
		var cpuSum, cpuHeld, heldSum, overlapSum int64
		for i, r := range rs {
			held = append(held, r.held)
			waited = append(waited, r.waited)
			if r.cpu > 0 && r.held > 0 {
				cpuSum += r.cpu
				cpuHeld += r.held
			}
			if r.rss > 0 {
				rss = append(rss, r.rss)
			}
			heldSum += r.held
			overlapSum += overlapped(r, i, all)
		}
		s.HeldP50MS, s.HeldP95MS = percentile(held, 50), percentile(held, 95)
		s.WaitedP50MS, s.WaitedP95MS = percentile(waited, 50), percentile(waited, 95)
		s.RSSP95KB = percentile(rss, 95)
		if cpuHeld > 0 {
			s.AvgCores = float64(cpuSum) / float64(cpuHeld)
		}
		if heldSum > 0 {
			s.Overlap = float64(overlapSum) / float64(heldSum)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Class < out[j].Class })
	return out
}

// overlapped is how many ms of r are covered by at least one other run in
// all (r itself is skipped by identity of its values and position).
func overlapped(r statRow, _ int, all []statRow) int64 {
	type iv struct{ a, b int64 }
	var ivs []iv
	skipped := false
	for _, o := range all {
		if !skipped && o == r {
			skipped = true
			continue
		}
		a, b := max(o.start, r.start), min(o.end, r.end)
		if b > a {
			ivs = append(ivs, iv{a, b})
		}
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].a < ivs[j].a })
	var total, curEnd int64
	curEnd = math.MinInt64
	for _, v := range ivs {
		a := max(v.a, curEnd)
		if v.b > a {
			total += v.b - a
		}
		curEnd = max(curEnd, v.b)
	}
	return total
}

// percentile is the nearest-rank p-th percentile of xs; 0 for none.
func percentile(xs []int64, p int) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(math.Ceil(float64(p) / 100 * float64(len(s))))
	return s[max(rank, 1)-1]
}

// pruneHistory deletes history older than HistoryKeep.
func pruneHistory(db *sql.DB, now time.Time) error {
	_, err := db.Exec(`DELETE FROM history WHERE ended<?`, now.Add(-HistoryKeep).UnixMilli())
	return err
}
