//go:build !linux && !darwin

package termpane

import (
	"errors"
	"os"
	"syscall"
)

var errUnsupported = errors.New("the terminal pane is not supported on this platform")

func openPty() (*os.File, string, error) { return nil, "", errUnsupported }
func setSize(*os.File, int, int) error   { return errUnsupported }
func sysProcAttr() *syscall.SysProcAttr  { return nil }
func hangup(int)                         {}
