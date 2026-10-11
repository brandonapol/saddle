package remote

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func logN(t *testing.T, a *Audit, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if err := a.Log(AuditEntry{Token: fmt.Sprintf("tok%03d", i), Addr: "127.0.0.1", Tool: "status", Decision: DecisionAllowed}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAuditRotates: past MaxBytes the log moves to audit.log.1 (older ones
// shift up), at most Keep old files are kept, every file stays 0600, and
// ReadAudit returns what is left oldest first with nothing out of order.
func TestAuditRotates(t *testing.T) {
	dir := t.TempDir()
	a, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	a.MaxBytes, a.Keep = 400, 2
	logN(t, a, 0, 40)

	for _, name := range []string{auditFile, auditFile + ".1", auditFile + ".2"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", name, fi.Mode().Perm())
		}
		if fi.Size() > 400+200 {
			t.Errorf("%s is %d bytes, past the 400 limit by more than a line", name, fi.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, auditFile+".3")); !os.IsNotExist(err) {
		t.Fatalf("audit.log.3 exists with Keep = 2 (%v)", err)
	}
	es, err := ReadAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) == 0 || len(es) >= 40 {
		t.Fatalf("ReadAudit returned %d entries; want some rotated away", len(es))
	}
	first := 40 - len(es)
	for i, e := range es {
		if want := fmt.Sprintf("tok%03d", first+i); e.Token != want {
			t.Fatalf("entry %d = %s, want %s (out of order or a gap)", i, e.Token, want)
		}
	}
}

// TestAuditFollowsRotationByAnotherProcess: two servers (an engine-run serve
// and an ssh stdio session) share the log. After one rotates, the other
// writes to the new audit.log, not the renamed one.
func TestAuditFollowsRotationByAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	a, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	a.MaxBytes = 100
	logN(t, a, 0, 3) // rotates
	if err := b.Log(AuditEntry{Token: "from-b", Decision: DecisionAllowed}); err != nil {
		t.Fatal(err)
	}
	cur, err := os.ReadFile(filepath.Join(dir, auditFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cur), "from-b") {
		t.Fatalf("b wrote into a rotated file; audit.log = %q", cur)
	}
}

func TestAuditDefaults(t *testing.T) {
	a, err := OpenAudit(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if a.MaxBytes != DefaultAuditMaxBytes || a.Keep != DefaultAuditKeep {
		t.Fatalf("defaults = %d, %d", a.MaxBytes, a.Keep)
	}
}

func TestReadAuditSkipsBadLines(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, auditFile), `{"token":"a","decision":"allowed","addr":"x"}`+"\nnot json\n"+`{"token":"b","decision":"denied","addr":"x"}`+"\n")
	es, err := ReadAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || es[0].Token != "a" || es[1].Token != "b" {
		t.Fatalf("ReadAudit = %+v", es)
	}
}

func TestAuditCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)
	a, err := OpenAudit(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, e := range []AuditEntry{
		{TS: now.Add(-3 * time.Hour), Token: "old", Addr: "127.0.0.1", Tool: "status", Decision: DecisionAllowed},
		{TS: now.Add(-time.Minute), Token: "phone", Addr: "127.0.0.1", Tool: "status", Decision: DecisionAllowed},
		{TS: now.Add(-time.Minute), Token: "phone", Addr: "127.0.0.1", Tool: "land", Decision: DecisionDenied, Reason: "token \"phone\" lacks the scope"},
		{TS: now, Token: "owner", Addr: "127.0.0.1", Tool: "kill", Decision: DecisionPending},
	} {
		if err := a.Log(e); err != nil {
			t.Fatal(err)
		}
	}
	_ = a.Close()

	out, err := run(t, noOpen, "audit")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TIME", "old", "phone", "land", "denied", "lacks the scope", "owner", "pending"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit output lacks %q:\n%s", want, out)
		}
	}
	if out, _ = run(t, noOpen, "audit", "--token", "phone"); strings.Contains(out, "owner") || !strings.Contains(out, "phone") {
		t.Errorf("--token phone:\n%s", out)
	}
	if out, _ = run(t, noOpen, "audit", "--denied"); strings.Contains(out, "status") || !strings.Contains(out, "land") {
		t.Errorf("--denied:\n%s", out)
	}
	if out, _ = run(t, noOpen, "audit", "--since", "1h"); strings.Contains(out, "old") || !strings.Contains(out, "owner") {
		t.Errorf("--since 1h:\n%s", out)
	}
	if out, _ = run(t, noOpen, "audit", "-n", "1"); strings.Contains(out, "phone") || !strings.Contains(out, "owner") {
		t.Errorf("-n 1:\n%s", out)
	}
	out, err = run(t, noOpen, "audit", "--json", "--tool", "land")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var e AuditEntry
	if len(lines) != 1 || json.Unmarshal([]byte(lines[0]), &e) != nil || e.Tool != "land" {
		t.Fatalf("--json --tool land = %q", out)
	}
}

func TestAuditCommandWithNoLog(t *testing.T) {
	t.Setenv(EnvDir, t.TempDir())
	out, err := run(t, noOpen, "audit")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no audit entries") {
		t.Fatalf("empty audit = %q", out)
	}
}
