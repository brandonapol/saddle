//go:build !linux && !darwin

package tui

// ttyPending is nil where saddle cannot ask the terminal for unread bytes;
// holds fall back to a plain wait.
func ttyPending() func() bool { return nil }
