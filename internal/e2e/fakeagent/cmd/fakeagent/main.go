// Command fakeagent is the scripted Claude Code stand-in for saddle's e2e
// tests; see package fakeagent.
package main

import (
	"os"

	"github.com/brandonapol/saddle/internal/e2e/fakeagent"
)

func main() { os.Exit(fakeagent.Main(os.Args[1:], os.Stdin, os.Stdout)) }
