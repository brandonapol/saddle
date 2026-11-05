package app

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// holdByChild starts `sleep 600` holding the flock on path (it inherits the
// locked open file) and returns it. pidPath, when set, is written with its
// pid as AcquireLock would.
func holdByChild(t *testing.T, path, pidPath string) *exec.Cmd {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	c := exec.Command("sleep", "600")
	c.ExtraFiles = []*os.File{f}
	must(t, c.Start())
	f.Close() // only the child holds it now
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	if pidPath == "" {
		pidPath = path
	}
	must(t, os.WriteFile(pidPath, []byte(strconv.Itoa(c.Process.Pid)+"\n"), 0o644))
	return c
}

// #789: the engine (or saddle up) takes train.lock in-process. A slow land
// under it must not get the engine SIGTERMed by another process's
// stealTrainLock, which would stop every watcher.
func TestStealTrainLockNeverSignalsTheEngine(t *testing.T) {
	for _, lock := range []string{"engine", "up"} {
		t.Run(lock, func(t *testing.T) {
			a := trainSetup(t)
			setGateTimeout(t, "10ms")
			var holder *exec.Cmd
			if lock == "engine" {
				must(t, os.WriteFile(a.lockPath(), []byte(LockEngine+"\n"), 0o644))
				holder = holdByChild(t, a.lockPath(), a.lockPIDPath())
			} else {
				holder = holdByChild(t, a.stateDir("up.lock"), "")
			}
			if !a.HoldsSessionLock(holder.Process.Pid) {
				t.Fatal("HoldsSessionLock missed the holder")
			}
			holdTrainLock(t, a, holder.Process.Pid, time.Now())
			time.Sleep(50 * time.Millisecond) // over twice gate_timeout
			a.stealTrainLock()
			if err := holder.Process.Signal(syscall.Signal(0)); err != nil {
				t.Fatalf("the engine was signalled: %v", err)
			}
			if lockStolen(t, a) {
				t.Fatal("stole train.lock from a live engine")
			}
			var st syscall.WaitStatus
			if pid, _ := syscall.Wait4(holder.Process.Pid, &st, syscall.WNOHANG, nil); pid != 0 {
				t.Fatalf("holder exited: %v", st)
			}
		})
	}
}

// A released engine lock protects nobody.
func TestHoldsSessionLockFalseOnceReleased(t *testing.T) {
	a := trainSetup(t)
	rel, err := a.AcquireLock(LockEngine)
	must(t, err)
	if !a.HoldsSessionLock(os.Getpid()) {
		t.Fatal("own engine lock not seen")
	}
	rel()
	if a.HoldsSessionLock(os.Getpid()) {
		t.Fatal("released lock still seen as held")
	}
}
