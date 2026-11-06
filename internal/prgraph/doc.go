// Package prgraph is the PR-graph engine of ADR 0009: one desired-state
// model, one change state machine and one pure Reconcile.
//
// A change is one unit of submitted work as the engine sees it: a fixed
// branch, its current head, its base, its dependency edges and, once
// published, its PR. The host stores intent (changes, edges, policy) and
// history (attempts); everything else is read from the forge and git every
// tick and passed in as ForgeSnapshot and GitFacts.
//
// Reconcile(Input) Plan is pure: no I/O, no clock other than Input.Now, no
// randomness, no package-level state. The same Input gives a byte-identical
// Plan whatever the order of its slices. Every effectful Action carries a
// Guard that the Executor checks with a compare-and-set, so a crash anywhere
// is healed by the next tick and there is no journal.
//
// The package imports only the standard library. It knows nothing of the
// host that drives it: Change.ID is an opaque string, and the host
// implements Source, Executor, Locker and Store.
package prgraph
