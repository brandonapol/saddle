package store

import "strings"

// EventsOfKinds returns every task's events of any of kinds, oldest first.
// It reads only those rows, through the events(kind, task) index, where
// Events(-1) and a filter in Go read the whole log (#778). INDEXED BY
// holds even when ANALYZE stats make one kind look unselective.
func (s *Store) EventsOfKinds(kinds ...string) ([]Event, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	q, args := eventsOfKindsQuery(kinds)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	return s.scanEvents(rows)
}

func eventsOfKindsQuery(kinds []string) (string, []any) {
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	return `SELECT ` + eventCols + ` FROM events INDEXED BY events_kind_task WHERE kind IN (?` + strings.Repeat(`,?`, len(kinds)-1) + `) ORDER BY id`, args
}
