// Package uplock is the per-repo session lock behind saddle up. It is a flock
// on a pid file: the kernel drops the lock when the holder dies, so a crashed
// session never wedges the next one, and the pid in the file lets a second
// session say who holds it and, with Takeover, stop it (#665).
package uplock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Options tune Acquire.
type Options struct {
	// Takeover stops a live holder (SIGTERM, then SIGKILL) instead of
	// refusing.
	Takeover bool
	// Grace is how long a SIGTERMed holder gets to exit before SIGKILL.
	// Zero means 10s.
	Grace time.Duration
}

// HeldError reports that a live session holds the lock.
type HeldError struct{ PID int }

func (e *HeldError) Error() string {
	return fmt.Sprintf("saddle up is already running for this repo (pid %d); "+
		"use that session, or run `saddle up --takeover` to stop it and take over", e.PID)
}

// Acquire takes the lock at path and returns the func that releases it. A lock
// whose holder is dead is taken over silently.
func Acquire(path string, o Options) (release func(), err error) {
	grace := o.Grace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	signalled := false
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			if err := errors.Join(f.Truncate(0), writeAt0(f, strconv.Itoa(os.Getpid())+"\n")); err != nil {
				f.Close()
				return nil, err
			}
			return func() {
				_ = f.Truncate(0)
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		pid := readPID(f)
		f.Close()
		if pid > 0 && pid != os.Getpid() && !alive(pid) {
			// Holder is gone but something it forked still has the file
			// open. The lock is stale: replace the file.
			if attempt < 3 {
				_ = os.Remove(path)
				continue
			}
		}
		if !o.Takeover {
			return nil, &HeldError{PID: pid}
		}
		if pid <= 0 || pid == os.Getpid() {
			// Holder not yet recorded (it is mid-acquire); give it a moment.
			time.Sleep(50 * time.Millisecond)
			if attempt > 100 {
				return nil, &HeldError{PID: pid}
			}
			continue
		}
		if !signalled {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			signalled = true
			if waitDead(pid, grace) {
				continue
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
			if waitDead(pid, 5*time.Second) {
				continue
			}
			return nil, fmt.Errorf("could not stop saddle up (pid %d)", pid)
		}
		time.Sleep(20 * time.Millisecond)
		if attempt > 500 {
			return nil, &HeldError{PID: pid}
		}
	}
}

func writeAt0(f *os.File, s string) error {
	_, err := f.WriteAt([]byte(s), 0)
	return err
}

func readPID(f *os.File) int {
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	return pid
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitDead(pid int, d time.Duration) bool {
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if !alive(pid) {
			return true
		}
	}
	return !alive(pid)
}
