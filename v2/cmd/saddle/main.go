// Command saddle2 is Saddle v2. It is named saddle2, and keeps its state in
// .saddle2/, until the cutover (#986), so v1 can build it.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/brandonapol/saddle/v2/internal/core"
)

// version is set at build time with -ldflags "-X main.version=...". Without
// it the build id comes from the VCS stamp Go records.
var version = ""

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(stdout, "saddle2", buildID())
		return 0
	}
	if len(args) == 1 && args[0] == "states" {
		fmt.Fprint(stdout, core.Markdown())
		return 0
	}
	fmt.Fprintln(stderr, "usage: saddle2 version | states")
	fmt.Fprintln(stderr, "Saddle v2 is under construction: see docs/v2/ARCHITECTURE.md.")
	return 2
}

// buildID identifies this binary. The engine stores it so that a newer
// binary can ask an older engine to hand over.
func buildID() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return rev + dirty
}
