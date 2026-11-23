// Package store owns Saddle's database: the schema, migrations and every SQL
// statement. It is the only package that writes under the state directory,
// apart from turn (attempt logs) and git (worktrees).
package store

// StateDir is the directory, under the repo root, that holds all of Saddle's
// state. It is .saddle2 until the cutover (#986) so v1 can run beside v2.
const StateDir = ".saddle2"
