//go:build linux || darwin

package termpane

import (
	"os"
	"syscall"
	"unsafe"
)

func ioctl(f *os.File, req, arg uintptr) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

// setSize sets the pty's window size, which also signals SIGWINCH to the
// foreground process group.
func setSize(f *os.File, cols, rows int) error {
	ws := struct{ row, col, x, y uint16 }{uint16(rows), uint16(cols), 0, 0}
	return ioctl(f, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

// sysProcAttr starts the child as a session leader with the pty as its
// controlling terminal (its stdin, fd 0).
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
}

// hangup sends SIGHUP to the shell's process group, as closing a terminal does.
func hangup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	_ = syscall.Kill(pid, syscall.SIGHUP)
}
