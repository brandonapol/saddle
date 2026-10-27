// Command opencode is fakeopencode as a binary named opencode.
package main

import (
	"os"

	"github.com/brandonapol/saddle/internal/agent/agenttest/fakeopencode"
)

func main() { os.Exit(fakeopencode.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
