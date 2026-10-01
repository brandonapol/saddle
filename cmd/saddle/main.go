package main

import (
	"os"

	"github.com/brandonapol/saddle/internal/cli"
)

func main() {
	if err := cli.Root().Execute(); err != nil {
		os.Exit(1)
	}
}
