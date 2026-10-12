// Package inhibit keeps the machine awake while saddle's autopilot runs
// (#409): Remote Control, and work left running overnight, need it awake.
// It holds systemd-inhibit on Linux and caffeinate on macOS.
package inhibit

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// Tool is a sleep inhibitor: a command that keeps the machine awake for as
// long as it runs.
type Tool struct {
	Name string
	Args []string
}

const why = "saddle autopilot is running"

// For returns goos's inhibitor if lookPath finds it.
func For(goos string, lookPath func(string) (string, error)) (Tool, bool) {
	var t Tool
	switch goos {
	case "linux":
		t = Tool{Name: "systemd-inhibit", Args: []string{"--what=sleep:idle", "--who=saddle", "--why=" + why, "--mode=block", "sleep", "infinity"}}
	case "darwin":
		t = Tool{Name: "caffeinate", Args: []string{"-i", "-s"}}
	default:
		return Tool{}, false
	}
	if _, err := lookPath(t.Name); err != nil {
		return Tool{}, false
	}
	return t, true
}

// Hint is how to get goos an inhibitor, for saddle doctor.
func Hint(goos string) string {
	switch goos {
	case "linux":
		return "install systemd (systemd-inhibit), or keep the machine from sleeping yourself while autopilot runs"
	case "darwin":
		return "caffeinate ships with macOS; check that /usr/bin is on PATH"
	}
	return "keep the machine from sleeping yourself while autopilot runs"
}

// Starter starts a held inhibitor and returns how to release it.
type Starter func(Tool) (release func(), err error)

// Exec runs t until released.
func Exec(t Tool) (func(), error) {
	cmd := exec.Command(t.Name, t.Args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}, nil
}

// Watch holds t while on reports true, checking every poll, until ctx
// ends. Errors from on or start go to logf, and are retried next poll.
func Watch(ctx context.Context, t Tool, on func() (bool, error), start Starter, poll time.Duration, logf func(string, ...any)) {
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	tk := time.NewTicker(poll)
	defer tk.Stop()
	last := ""
	report := func(err error) {
		if msg := err.Error(); msg != last {
			last = msg
			logf("sleep inhibitor: %v", err)
		}
	}
	for {
		want, err := on()
		if err != nil {
			report(err)
		}
		switch {
		case err != nil:
		case want && release == nil:
			if release, err = start(t); err != nil {
				release = nil
				report(err)
			} else {
				logf("sleep inhibitor: holding %s while autopilot runs", t.Name)
			}
		case !want && release != nil:
			release()
			release = nil
			logf("sleep inhibitor: released %s", t.Name)
		}
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}
