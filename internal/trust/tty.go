package trust

import "os"

// IsTerminal reports whether f is a terminal a person can answer the prompt
// on. Unlike a character-device check it says no for /dev/null, which is
// what stdin is under go test, cron and most CI.
func IsTerminal(f *os.File) bool { return isTerminal(f.Fd()) }
