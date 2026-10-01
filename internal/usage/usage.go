// Package usage collects token usage from agent transcripts and aggregates it
// into per-minute buckets keyed by task and model.
//
// It is a library only: nothing here touches SQLite. Buckets returned by
// Collector.Poll are deltas (tokens seen since the previous poll), so a
// persistence layer can upsert-add them into a usage table keyed by
// (minute, task, model).
package usage

import (
	"sort"
	"time"
)

// Tokens is a set of token counts.
type Tokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// Add returns t + o.
func (t Tokens) Add(o Tokens) Tokens {
	return Tokens{
		Input:         t.Input + o.Input,
		Output:        t.Output + o.Output,
		CacheRead:     t.CacheRead + o.CacheRead,
		CacheCreation: t.CacheCreation + o.CacheCreation,
	}
}

// Total is the sum of all token kinds.
func (t Tokens) Total() int64 { return t.Input + t.Output + t.CacheRead + t.CacheCreation }

// Record is one usage observation parsed from a transcript line.
type Record struct {
	// ID identifies the upstream message; records with a repeated non-empty ID
	// within one session are counted once.
	ID     string
	Time   time.Time
	Model  string
	Tokens Tokens
}

// Key identifies a bucket.
type Key struct {
	Minute time.Time `json:"minute"` // UTC, truncated to the minute
	Task   string    `json:"task"`
	Model  string    `json:"model"`
}

// Bucket is the usage for one Key.
type Bucket struct {
	Key
	Tokens
	Messages int64 `json:"messages"`
}

// Aggregator accumulates records into buckets. It is not safe for concurrent
// use.
type Aggregator struct {
	m map[Key]*Bucket
}

// NewAggregator returns an empty Aggregator.
func NewAggregator() *Aggregator { return &Aggregator{m: map[Key]*Bucket{}} }

// Add folds a record for task into its minute bucket.
func (a *Aggregator) Add(task string, r Record) {
	k := Key{Minute: r.Time.UTC().Truncate(time.Minute), Task: task, Model: r.Model}
	b := a.m[k]
	if b == nil {
		b = &Bucket{Key: k}
		a.m[k] = b
	}
	b.Tokens = b.Add(r.Tokens)
	b.Messages++
}

// Len reports the number of buckets held.
func (a *Aggregator) Len() int { return len(a.m) }

// Flush returns the buckets sorted by minute, task, model and empties the
// aggregator.
func (a *Aggregator) Flush() []Bucket {
	out := make([]Bucket, 0, len(a.m))
	for _, b := range a.m {
		out = append(out, *b)
	}
	a.m = map[Key]*Bucket{}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if !x.Minute.Equal(y.Minute) {
			return x.Minute.Before(y.Minute)
		}
		if x.Task != y.Task {
			return x.Task < y.Task
		}
		return x.Model < y.Model
	})
	return out
}
