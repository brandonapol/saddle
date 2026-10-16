// Command codex is fakecodex as a binary named codex.
package main

import (
	"os"

	"github.com/brandonapol/saddle/internal/agent/agenttest/fakecodex"
)

func main() { os.Exit(fakecodex.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
