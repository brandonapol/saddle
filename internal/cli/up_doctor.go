package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/doctor"
)

// upDoctorTimeout bounds the startup checks so a hung gh call can't block up.
const upDoctorTimeout = 10 * time.Second

// upDoctor runs the doctor checks once for saddle up and prints one status
// line. It returns an error naming the failing checks, and nothing else is
// started. A run that exceeds timeout counts as one warning. skip bypasses
// the checks entirely.
func upDoctor(out io.Writer, skip bool, timeout time.Duration, run func() []doctor.Result) error {
	if skip {
		return nil
	}
	done := make(chan []doctor.Result, 1)
	go func() { done <- run() }()
	var rs []doctor.Result
	select {
	case rs = <-done:
	case <-time.After(timeout):
		fmt.Fprintf(out, "doctor: 1 warning, checks timed out after %s (run saddle doctor)\n", timeout)
		return nil
	}
	var failed []string
	warns := 0
	for _, r := range rs {
		switch r.Status {
		case doctor.Fail:
			failed = append(failed, r.Name)
		case doctor.Warn:
			warns++
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("doctor: failing checks: %s\nrun `saddle doctor` for the fix for each, or pass --skip-doctor to start anyway", strings.Join(failed, ", "))
	}
	switch warns {
	case 0:
		fmt.Fprintln(out, "doctor: ok")
	case 1:
		fmt.Fprintln(out, "doctor: 1 warning (run saddle doctor)")
	default:
		fmt.Fprintf(out, "doctor: %d warnings (run saddle doctor)\n", warns)
	}
	return nil
}
