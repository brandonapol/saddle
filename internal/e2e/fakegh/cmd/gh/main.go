// Command gh is the fake GitHub CLI for saddle's e2e tests. Put the built
// binary on PATH as gh; see package fakegh.
package main

import (
	"os"

	"github.com/brandonapol/saddle/internal/e2e/fakegh"
)

func main() {
	wd, _ := os.Getwd()
	os.Exit(fakegh.Main(wd, os.Args[1:], os.Stdout, os.Stderr))
}
