package store

import "database/sql"

// SymbolClaim is a task's hold on one symbol of a file it shares with other
// tasks (#591, docs/symbol-claims.md "Rule 1"). The rows tell a later write
// whom to compare against; the live diffs stay the source of truth.
type SymbolClaim struct {
	Task, Path, Symbol, Kind string
}

// SymbolLines is the kind of a row with no symbol: the task changed the
// file, maybe only outside every symbol. It marks the task as one to compare
// against and never clashes.
const SymbolLines = "lines"

// SymbolClaims returns the symbol claims on path held by live tasks. Rows
// of landed, killed and no-change tasks are ignored, as path claims are.
func (s *Store) SymbolClaims(path string) ([]SymbolClaim, error) {
	rows, err := s.db.Query(`SELECT c.task, c.path, c.symbol, c.kind FROM symbol_claims c JOIN tasks t ON t.id = c.task
		WHERE c.path = ? AND t.status NOT IN (?, ?, ?) ORDER BY c.task, c.symbol, c.kind`, path, Landed, Killed, NoChange)
	if err != nil {
		return nil, err
	}
	return scanSymbolClaims(rows)
}

func scanSymbolClaims(rows *sql.Rows) ([]SymbolClaim, error) {
	defer rows.Close()
	var out []SymbolClaim
	for rows.Next() {
		var c SymbolClaim
		if err := rows.Scan(&c.Task, &c.Path, &c.Symbol, &c.Kind); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimSymbols replaces task's symbol claims on path with want, in one
// transaction with a check against every other live task's rows there: when
// another task already holds one of want's symbols (same name and kind),
// nothing is written and that row comes back as clash. This closes the race
// between two writers that both read the other's diff before either wrote.
func (s *Store) ClaimSymbols(task, path string, want []SymbolClaim) (clash *SymbolClaim, err error) {
	err = s.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT c.task, c.path, c.symbol, c.kind FROM symbol_claims c JOIN tasks t ON t.id = c.task
			WHERE c.path = ? AND c.task != ? AND t.status NOT IN (?, ?, ?) ORDER BY c.task, c.symbol, c.kind`,
			path, task, Landed, Killed, NoChange)
		if err != nil {
			return err
		}
		others, err := scanSymbolClaims(rows)
		if err != nil {
			return err
		}
		for _, w := range want {
			for _, o := range others {
				if w.Symbol != "" && o.Symbol == w.Symbol && o.Kind == w.Kind {
					clash = &o
					return nil
				}
			}
		}
		if _, err := tx.Exec(`DELETE FROM symbol_claims WHERE task = ? AND path = ?`, task, path); err != nil {
			return err
		}
		for _, w := range want {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO symbol_claims(task, path, symbol, kind) VALUES(?, ?, ?, ?)`,
				task, path, w.Symbol, w.Kind); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return clash, nil
}
