//go:build !linux

package runq

import (
	"os"
	"os/exec"
	"time"
)

// dieWithParent is Linux-only (Pdeathsig); elsewhere an orphaned child keeps
// running after its lease is reaped.
func dieWithParent(*exec.Cmd) {}

// rusage is recorded on Linux only for now.
func rusage(*os.ProcessState) (time.Duration, int64) { return 0, 0 }
