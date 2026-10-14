package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/sre"
)

// #550: investigators state and verify root causes through saddle sre; the
// same agent can't verify its own, and close lifts the guard.
func TestSREGuardCommands(t *testing.T) {
	a := automergeRepo(t)
	t.Chdir(a.Root)
	t.Setenv("SADDLE_TASK", "")

	if out, err := runCmd(t, sreCmd(), "incident"); err != nil || !strings.Contains(out, "No SRE incident") {
		t.Fatalf("incident with none: %v\n%s", err, out)
	}
	if err := sre.OpenIncident(a.Root, &sre.Incident{ID: "api-500s", Opened: time.Now(), Investigators: []string{"t1", "t2"}}); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, sreCmd(), "root-cause", "--task", "t1", "--evidence", "errors start at deploy 4f2a", "bad deploy 4f2a")
	if err != nil || !strings.Contains(out, "root cause 1") {
		t.Fatalf("root-cause: %v\n%s", err, out)
	}
	if _, err := runCmd(t, sreCmd(), "verify", "--task", "t1", "--evidence", "again", "1"); err == nil || !strings.Contains(err.Error(), "different agent") {
		t.Fatalf("self-verify: %v", err)
	}
	if out, _ := runCmd(t, sreCmd(), "incident"); !strings.Contains(out, "read-only") {
		t.Fatalf("incident before verification:\n%s", out)
	}
	if _, err := runCmd(t, sreCmd(), "verify", "--task", "t2", "1"); err == nil {
		t.Fatal("verify without evidence passed")
	}
	if out, err := runCmd(t, sreCmd(), "verify", "--task", "t2", "--evidence", "canary on 4f29 is clean", "1"); err != nil || !strings.Contains(out, "unlocked") {
		t.Fatalf("verify: %v\n%s", err, out)
	}
	if out, _ := runCmd(t, sreCmd(), "incident"); !strings.Contains(out, "verified by t2") {
		t.Fatalf("incident after verification:\n%s", out)
	}
	if _, err := runCmd(t, sreCmd(), "refute", "--task", "t2", "--evidence", "errors continue after rollback", "1"); err != nil {
		t.Fatal(err)
	}
	if out, _ := runCmd(t, sreCmd(), "incident"); !strings.Contains(out, "refuted by t2") || !strings.Contains(out, "read-only") {
		t.Fatalf("incident after refute:\n%s", out)
	}
	out, err = runCmd(t, sreCmd(), "close")
	if err != nil || !strings.Contains(out, "unresolved") {
		t.Fatalf("close: %v\n%s", err, out)
	}
	inc, _ := sre.LoadIncident(a.Root)
	if inc.Open() {
		t.Fatal("close left the incident open")
	}
}

// #574: closing with a verified root cause writes a postmortem naming it;
// closing without one writes an unresolved postmortem.
func TestSRECloseWritesPostmortem(t *testing.T) {
	for _, verified := range []bool{true, false} {
		a := automergeRepo(t)
		t.Chdir(a.Root)
		t.Setenv("SADDLE_TASK", "")
		if err := sre.OpenIncident(a.Root, &sre.Incident{ID: "api-500s", Summary: "API 500s", Opened: time.Now(), Investigators: []string{"t1", "t2"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, sreCmd(), "root-cause", "--task", "t1", "--evidence", "e", "bad deploy 4f2a"); err != nil {
			t.Fatal(err)
		}
		if verified {
			if _, err := runCmd(t, sreCmd(), "verify", "--task", "t2", "--evidence", "canary clean", "1"); err != nil {
				t.Fatal(err)
			}
		}
		out, err := runCmd(t, sreCmd(), "close")
		if err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join(a.Root, "docs", "postmortems", "*-api-500s.md"))
		if len(files) != 1 || !strings.Contains(out, files[0]) {
			t.Fatalf("verified=%v files %v, output:\n%s", verified, files, out)
		}
		b, _ := os.ReadFile(files[0])
		body := string(b)
		if verified && (!strings.Contains(body, "bad deploy 4f2a") || !strings.Contains(body, "Verified by t2")) {
			t.Fatalf("verified postmortem:\n%s", body)
		}
		if !verified && (!strings.Contains(body, "Unknown: no root cause") || strings.Contains(out, "unresolved") == false) {
			t.Fatalf("unresolved postmortem:\n%s\n%s", body, out)
		}
	}
}
