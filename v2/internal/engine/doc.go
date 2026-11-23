// Package engine is the one process per repo that observes the database, the
// forge and running turns, asks core.Plan what to do, and applies each action
// idempotently. Nothing talks to it; it reads the database.
package engine
