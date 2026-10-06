//go:build !linux

package runq

import "os/exec"

// dieWithParent is Linux-only (Pdeathsig); elsewhere an orphaned child keeps
// running after its lease is reaped.
func dieWithParent(*exec.Cmd) {}
