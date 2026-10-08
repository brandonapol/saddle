package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/runq"
)

// runqEnv points the queue at a private file in mode and catches exits.
func runqEnv(t *testing.T, mode string) (db string, code *int) {
	t.Helper()
	db = filepath.Join(t.TempDir(), "runq.db")
	t.Setenv(runq.EnvPath, db)
	t.Setenv(runq.EnvBypass, mode)
	t.Setenv(runq.EnvLease, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(runq.EnvProcRoot, t.TempDir()) // no probe: the load gate fails open
	t.Chdir(t.TempDir()) // outside any repo
	code = new(int)
	*code = -1
	old := exitFn
	exitFn = func(c int) { *code = c }
	t.Cleanup(func() { exitFn = old })
	return db, code
}

// hold takes a go-test slot as t83 in an enforcing queue.
func hold(t *testing.T, db string) *runq.Lease {
	t.Helper()
	q, err := runq.Open(runq.Options{Path: db, Mode: runq.ModeEnforce, Slots: map[string]int{"go-test": 1},
		Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	l, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Label: "t83", Cmd: "make check"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release() })
	return l
}

func TestRunForwardsOutputAndExitCode(t *testing.T) {
	_, code := runqEnv(t, "")
	out, err := runCmd(t, heavyRunCmd(), "--class", "go-test", "--", "sh", "-c", "echo hi; exit 7")
	if err != nil || *code != 7 || !strings.Contains(out, "hi") {
		t.Fatalf("code %d err %v out %q", *code, err, out)
	}
	*code = -1
	if out, err := runCmd(t, heavyRunCmd(), "--class", "go-test", "--", "true"); err != nil || *code != -1 {
		t.Fatalf("success: code %d err %v out %q", *code, err, out)
	}
}

func TestRunForwardsStdin(t *testing.T) {
	runqEnv(t, "")
	c := heavyRunCmd()
	c.SetIn(strings.NewReader("from stdin"))
	if out, err := runCmd(t, c, "--class", "go-test", "--", "cat"); err != nil || !strings.Contains(out, "from stdin") {
		t.Fatalf("err %v out %q", err, out)
	}
}

func TestRunMissingCommandExits127(t *testing.T) {
	_, code := runqEnv(t, "")
	if out, _ := runCmd(t, heavyRunCmd(), "--class", "go-test", "--", "no-such-command-saddle"); *code != 127 {
		t.Fatalf("code %d out %q", *code, out)
	}
}

// TestRunPassesLeaseToken: the command sees the lease token, so a nested
// saddle run rides on it.
func TestRunPassesLeaseToken(t *testing.T) {
	runqEnv(t, "enforce")
	out, err := runCmd(t, heavyRunCmd(), "--class", "go-test", "--", "sh", "-c", `echo "tok=$SADDLE_RUNQ_LEASE"`)
	if err != nil || !regexp.MustCompile(`tok=[0-9a-f]{24}`).MatchString(out) {
		t.Fatalf("err %v out %q", err, out)
	}
}

// TestRunWaitMaxExits75: giving up names the holder, says not to retry in a
// loop and exits EX_TEMPFAIL.
func TestRunWaitMaxExits75(t *testing.T) {
	db, code := runqEnv(t, "enforce")
	hold(t, db)
	out, _ := runCmd(t, heavyRunCmd(), "--class", "go-test", "--wait-max", "300ms", "--", "true")
	if *code != 75 || !strings.Contains(out, "queued: position 1 of 1 for go-test, holder t83") ||
		!strings.Contains(out, "t83 (make check") || !strings.Contains(out, "not retry") {
		t.Fatalf("code %d out %q", *code, out)
	}
}

// TestRunBypass: SADDLE_RUNQ=off runs at once even with the class full.
func TestRunBypass(t *testing.T) {
	db, _ := runqEnv(t, "enforce")
	hold(t, db)
	t.Setenv(runq.EnvBypass, "off")
	if out, err := runCmd(t, heavyRunCmd(), "--class", "go-test", "--wait-max", "5s", "--", "echo", "ran"); err != nil || !strings.Contains(out, "ran") || strings.Contains(out, "queued") {
		t.Fatalf("err %v out %q", err, out)
	}
}

func TestRunUsage(t *testing.T) {
	runqEnv(t, "")
	for _, args := range [][]string{{"--", "true"}, {"--class", "go-test"}, {"--class", "go-test", "--prio", "urgent", "--", "true"}} {
		if out, err := runCmd(t, heavyRunCmd(), args...); err == nil {
			t.Errorf("saddle run %v succeeded: %q", args, out)
		}
	}
}

func TestParsePrio(t *testing.T) {
	for in, want := range map[string]int{"train": runq.PrioGate, "gate": runq.PrioGate, "worker": runq.PrioWorker, "background": runq.PrioBackground, "15": 15} {
		if got, err := parsePrio(in); err != nil || got != want {
			t.Errorf("parsePrio(%q) = %d, %v", in, got, err)
		}
	}
}

func TestRunqStatusJSON(t *testing.T) {
	db, _ := runqEnv(t, "enforce")
	l := hold(t, db)
	out, err := runCmd(t, runqCmd(), "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st runqStatusJSON
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if st.Mode != "enforce" || len(st.Classes) != 1 || st.Classes[0].Class != "go-test" ||
		len(st.Classes[0].Holders) != 1 || st.Classes[0].Holders[0].Label != "t83" || st.Classes[0].Holders[0].Lease != l.Token() {
		t.Fatalf("status %+v", st)
	}
	text, err := runCmd(t, runqCmd(), "status")
	if err != nil || !strings.Contains(text, "mode: enforce") || !strings.Contains(text, "go-test: 1/1 slots busy, 0 waiting") ||
		!strings.Contains(text, "running t83") || !strings.Contains(text, l.Token()[:8]) {
		t.Fatalf("err %v text %q", err, text)
	}
}

func TestRunqSlotsDrainKill(t *testing.T) {
	db, _ := runqEnv(t, "enforce")
	l := hold(t, db)
	must := func(args ...string) string {
		t.Helper()
		out, err := runCmd(t, runqCmd(), args...)
		if err != nil {
			t.Fatalf("saddle runq %v: %v\n%s", args, err, out)
		}
		return out
	}
	must("slots", "go-test", "3")
	if s := must("status"); !strings.Contains(s, "go-test: 1/3 slots busy") {
		t.Fatalf("after slots:\n%s", s)
	}
	must("drain")
	if s := must("status"); !strings.Contains(s, "go-test: 1/0 slots busy") || !strings.Contains(s, "drained") {
		t.Fatalf("after drain:\n%s", s)
	}
	must("kill", l.Token()[:8])
	if s := must("status"); !strings.Contains(s, "go-test: 0/0 slots busy") {
		t.Fatalf("after kill:\n%s", s)
	}
	for _, bad := range [][]string{{"kill", "nope"}, {"slots", "go-test", "-1"}, {"slots", "go-test", "many"}, {"slots", "go-test"}} {
		if out, err := runCmd(t, runqCmd(), bad...); err == nil {
			t.Errorf("saddle runq %v succeeded: %q", bad, out)
		}
	}
}

func TestRootRegistersRunAndRunq(t *testing.T) {
	for _, name := range []string{"run", "runq"} {
		if c, _, err := Root().Find([]string{name}); err != nil || c.Name() != name {
			t.Errorf("saddle %s not registered: %v", name, err)
		}
	}
}
