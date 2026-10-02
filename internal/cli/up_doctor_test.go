package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/doctor"
)

func fixed(rs ...doctor.Result) func() []doctor.Result {
	return func() []doctor.Result { return rs }
}

func TestUpDoctorAllOK(t *testing.T) {
	var out bytes.Buffer
	err := upDoctor(&out, false, time.Second, fixed(doctor.Result{Name: "tmux", Status: doctor.OK}))
	if err != nil || strings.TrimSpace(out.String()) != "doctor: ok" {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
}

func TestUpDoctorWarningsStartAndPrintLine(t *testing.T) {
	var out bytes.Buffer
	err := upDoctor(&out, false, time.Second, fixed(
		doctor.Result{Name: "tmux", Status: doctor.Warn},
		doctor.Result{Name: "claude", Status: doctor.Warn},
		doctor.Result{Name: "config", Status: doctor.OK},
	))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "doctor: 2 warnings (run saddle doctor)" {
		t.Fatalf("got %q", got)
	}
	out.Reset()
	_ = upDoctor(&out, false, time.Second, fixed(doctor.Result{Name: "tmux", Status: doctor.Warn}))
	if got := strings.TrimSpace(out.String()); got != "doctor: 1 warning (run saddle doctor)" {
		t.Fatalf("got %q", got)
	}
}

func TestUpDoctorFailRefusesNamingChecks(t *testing.T) {
	var out bytes.Buffer
	err := upDoctor(&out, false, time.Second, fixed(
		doctor.Result{Name: "git remote", Status: doctor.Fail},
		doctor.Result{Name: "gh auth", Status: doctor.Fail},
		doctor.Result{Name: "tmux", Status: doctor.Warn},
	))
	if err == nil {
		t.Fatal("expected refusal")
	}
	for _, want := range []string{"git remote", "gh auth", "saddle doctor", "--skip-doctor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "tmux") {
		t.Errorf("error names a non-failing check: %q", err)
	}
}

func TestUpDoctorSkipDoesNotRun(t *testing.T) {
	var out bytes.Buffer
	ran := false
	err := upDoctor(&out, true, time.Second, func() []doctor.Result {
		ran = true
		return []doctor.Result{{Name: "x", Status: doctor.Fail}}
	})
	if err != nil || ran {
		t.Fatalf("err=%v ran=%v", err, ran)
	}
}

func TestUpDoctorSlowCheckTimesOutAsWarn(t *testing.T) {
	var out bytes.Buffer
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	err := upDoctor(&out, false, 50*time.Millisecond, func() []doctor.Result {
		<-block
		return nil
	})
	if err != nil {
		t.Fatalf("timeout must not refuse: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("did not time out")
	}
	if got := out.String(); !strings.Contains(got, "1 warning") || !strings.Contains(got, "timed out") {
		t.Fatalf("got %q", got)
	}
}

func TestUpCmdHasSkipDoctorFlag(t *testing.T) {
	if upCmd().Flags().Lookup("skip-doctor") == nil {
		t.Fatal("missing --skip-doctor")
	}
}
