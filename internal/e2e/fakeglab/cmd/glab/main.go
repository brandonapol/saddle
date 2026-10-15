// Command glab is the fake GitLab CLI for saddle's e2e tests. Put the built
// binary on PATH as glab; see package fakeglab.
package main

import (
	"os"

	"github.com/brandonapol/saddle/internal/e2e/fakeglab"
)

func main() {
	wd, _ := os.Getwd()
	os.Exit(fakeglab.Main(wd, os.Args[1:], os.Stdout, os.Stderr))
}
