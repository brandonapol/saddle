package termpane

import (
	"os"
	"strconv"
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
	var unlock int32
	if err := ioctl(f, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		f.Close()
		return nil, "", err
	}
	var n uint32
	if err := ioctl(f, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		f.Close()
		return nil, "", err
	}
	return f, "/dev/pts/" + strconv.FormatUint(uint64(n), 10), nil
}
