package runq

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// dieWithParent kills the child if the process holding its lease dies, so a
// reaped lease never leaves an orphaned heavy run behind.
func dieWithParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// rusage is the CPU time and peak RSS (kB) wait4 reported for the child.
func rusage(ps *os.ProcessState) (time.Duration, int64) {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0, 0
	}
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	return cpu, ru.Maxrss // kilobytes on Linux
}
