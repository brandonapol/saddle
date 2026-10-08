package tui

import "syscall"

const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlReadable   = 0x4004667f // FIONREAD, which package syscall lacks
)
