package cli

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/doctor"
)

// e2eLeakCheck (#692) reports processes the e2e harness tagged with
// SADDLE_E2E_DIR whose dir is gone or stale, and abandoned e2etmux socket
// dirs, so a leak shows up before the load average does.
func e2eLeakCheck(procs, socks int) doctor.Result {
	const name = "leaked e2e processes"
	if procs == 0 && socks == 0 {
		return doctor.Result{Name: name, Status: doctor.OK, Detail: "none"}
	}
	return doctor.Result{Name: name, Status: doctor.Warn,
		Detail: fmt.Sprintf("%d leaked test processes (tmux servers, fake agents), %d abandoned e2etmux dirs", procs, socks),
		Fix:    "run saddle up: its sweep kills them and removes the dirs"}
}

func runE2ELeakCheck() doctor.Result {
	return e2eLeakCheck(app.E2ELeaks(app.GateSweepAge))
}
