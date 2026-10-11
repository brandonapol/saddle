package remote

import (
	"context"
	"slices"
	"strings"
	"time"
)

// Long-poll bounds for wait_needs_you.
const (
	DefaultWait = 2 * time.Minute
	MaxWait     = 10 * time.Minute
	// defaultPoll is how often a wait re-reads the needs-you queue.
	defaultPoll = 2 * time.Second
)

// WaitIn is wait_needs_you's input.
type WaitIn struct {
	Cursor         string `json:"cursor,omitempty" jsonschema:"the cursor the last needs_you or wait_needs_you returned; omit it to wait for items newer than now"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait; default 120, at most 600"`
}

// WaitOut is wait_needs_you's result.
type WaitOut struct {
	New      []NeedsYouItem `json:"new" jsonschema:"items the cursor hadn't seen"`
	Items    []NeedsYouItem `json:"items" jsonschema:"everything waiting now"`
	Cursor   string         `json:"cursor" jsonschema:"pass this to the next wait_needs_you"`
	TimedOut bool           `json:"timed_out,omitempty"`
}

func waitTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return DefaultWait
	}
	return min(time.Duration(seconds)*time.Second, MaxWait)
}

// cursorOf is the cursor for a set of items: their ids. Items that leave
// the queue drop out of it, so one that comes back counts as new.
func cursorOf(items []NeedsYouItem) string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	slices.Sort(ids)
	if len(ids) == 0 {
		return "-" // seen: nothing; distinct from "no cursor"
	}
	return strings.Join(ids, ",")
}

func newSince(cursor string, items []NeedsYouItem) []NeedsYouItem {
	seen := strings.Split(cursor, ",")
	out := []NeedsYouItem{}
	for _, it := range items {
		if !slices.Contains(seen, it.ID) {
			out = append(out, it)
		}
	}
	return out
}

// waitNeedsYou blocks until the queue holds an item the cursor hasn't
// seen, the timeout passes, or ctx ends (the client went away). Without a
// cursor it waits for items newer than its first read.
func waitNeedsYou(ctx context.Context, src Source, in WaitIn, poll time.Duration) (WaitOut, error) {
	if poll <= 0 {
		poll = defaultPoll
	}
	deadline := time.NewTimer(waitTimeout(in.TimeoutSeconds))
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	cursor := in.Cursor
	for {
		items, err := src.NeedsYou()
		if err != nil {
			return WaitOut{}, err
		}
		if items == nil {
			items = []NeedsYouItem{}
		}
		if cursor == "" {
			cursor = cursorOf(items)
		}
		out := WaitOut{New: newSince(cursor, items), Items: items, Cursor: cursorOf(items)}
		if len(out.New) > 0 {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-deadline.C:
			out.TimedOut = true
			return out, nil
		case <-tick.C:
		}
	}
}
