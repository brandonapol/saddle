package runq

import (
	"os/exec"
	"syscall"
)

// dieWithParent kills the child if the process holding its lease dies, so a
// reaped lease never leaves an orphaned heavy run behind.
func dieWithParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
