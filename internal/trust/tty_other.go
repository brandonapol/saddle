//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package trust

// isTerminal says no where saddle can't tell: callers then require --trust.
func isTerminal(uintptr) bool { return false }
