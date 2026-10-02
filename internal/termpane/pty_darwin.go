package termpane

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// openPty opens a new pseudo-terminal pair: the controller side and the path
// of the terminal side.
func openPty() (*os.File, string, error) {
	f, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	if err := ioctl(f, syscall.TIOCPTYGRANT, 0); err != nil {
		f.Close()
		return nil, "", err
	}
	if err := ioctl(f, syscall.TIOCPTYUNLK, 0); err != nil {
		f.Close()
		return nil, "", err
	}
	name := make([]byte, 128)
	if err := ioctl(f, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		f.Close()
		return nil, "", err
	}
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	return f, string(name), nil
}
