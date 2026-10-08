package tui

import "syscall"

const (
	ioctlGetTermios = syscall.TCGETS
	ioctlReadable   = syscall.TIOCINQ
)
