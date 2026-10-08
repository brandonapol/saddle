//go:build linux || darwin

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

// ttyPending reports unread bytes on standard input when it is the terminal
// Bubble Tea reads, for mouseScrub's idle check (#249). Nil otherwise: Bubble
// Tea then reads /dev/tty, and holds fall back to a plain wait.
func ttyPending() func() bool {
	fd := os.Stdin.Fd()
	var t syscall.Termios
	if ioctl(fd, ioctlGetTermios, unsafe.Pointer(&t)) != nil {
		return nil
	}
	return func() bool {
		var n int32
		return ioctl(fd, ioctlReadable, unsafe.Pointer(&n)) == nil && n > 0
	}
}

func ioctl(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}
