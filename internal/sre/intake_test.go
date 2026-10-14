package sre

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseIncidentPaths(t *testing.T) {
	inc := ParseIncident(`API 500s since deploy v2.3.1.
panic: runtime error: invalid memory address
github.com/acme/shop/internal/billing/invoice.go:142 +0x1d
/app/internal/http/handler.go:88
see https://api.example.com/healthz and ./cmd/server/main.go`)
	want := []string{
		"github.com/acme/shop/internal/billing/invoice.go",
		"/app/internal/http/handler.go",
		"./cmd/server/main.go",
	}
	if !reflect.DeepEqual(inc.Paths, want) {
		t.Fatalf("paths = %q, want %q", inc.Paths, want)
	}
	if !strings.Contains(inc.Summary, "API 500s") {
		t.Fatalf("summary = %q", inc.Summary)
	}
}

type fakeRunner struct {
	calls [][]string
	out   map[string]string
	fail  map[string]bool
}

func (f *fakeRunner) Run(_ context.Context, _ string, argv []string) (string, error) {
	f.calls = append(f.calls, argv)
	if f.fail[argv[0]] {
		return f.out[argv[0]], errors.New("exit status 1")
	}
	return f.out[argv[0]], nil
}

func TestRunChecks(t *testing.T) {
	c, err := Parse([]byte(`
[credentials.prod_db]
env = "SRE_TEST_PROD_DB"
[credentials.cluster]
ref = "kube-context:prod"
[endpoints.api]
url = "https://api.example.com/healthz"

[[check]]
name = "db-population"
command = ["psql", "-c", "select 1"]
expect = "1"
credentials = ["prod_db"]

[[check]]
name = "pods"
command = ["kubectl", "get", "pods"]
expect = "all Running"
credentials = ["cluster"]

[[check]]
name = "deps"
command = ["npm", "ls"]
expect = "exit 0"

[[check]]
name = "api-health"
surface = "endpoint:api"
expect = "HTTP 200"
`))
	if err != nil {
		t.Fatal(err)
	}
	ghp := "gh" + "p_abcdefghijklmnopqrstuvwxyz0123456789"
	r := &fakeRunner{
		out:  map[string]string{"kubectl": "web-1 CrashLoopBackOff " + ghp, "npm": "ok"},
		fail: map[string]bool{"kubectl": true},
	}
	t.Setenv("SRE_TEST_PROD_DB", "")
	res := RunChecks(context.Background(), c, ".", r)
	if len(res) != 4 {
		t.Fatalf("results = %+v", res)
	}
	got := map[string]CheckResult{}
	for _, x := range res {
		got[x.Name] = x
	}
	if s := got["db-population"]; s.Status != CheckSkipped || !strings.Contains(s.Note, "SRE_TEST_PROD_DB") {
		t.Fatalf("db-population = %+v, want skipped for unset env", s)
	}
	if s := got["pods"]; s.Status != CheckFailed || !strings.Contains(s.Output, "CrashLoopBackOff") {
		t.Fatalf("pods = %+v", s)
	}
	if strings.Contains(got["pods"].Output, ghp) {
		t.Fatal("check output leaks a secret")
	}
	if s := got["deps"]; s.Status != CheckOK {
		t.Fatalf("deps = %+v", s)
	}
	if s := got["api-health"]; s.Status != CheckManual || !strings.Contains(s.Note, "api.example.com") {
		t.Fatalf("api-health = %+v", s)
	}
	if len(r.calls) != 2 {
		t.Fatalf("ran %q, want only kubectl and npm", r.calls)
	}

	t.Setenv("SRE_TEST_PROD_DB", "postgres://x")
	r.calls = nil
	res = RunChecks(context.Background(), c, ".", r)
	if res[0].Status != CheckOK || len(r.calls) != 3 {
		t.Fatalf("with env set: %+v, calls %q", res[0], r.calls)
	}
}

func TestRunChecksNilConfig(t *testing.T) {
	if res := RunChecks(context.Background(), nil, ".", &fakeRunner{}); len(res) != 0 {
		t.Fatalf("res = %+v", res)
	}
}

func TestExecRunnerTruncates(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), t.TempDir(), []string{"sh", "-c", "head -c 10000 /dev/zero | tr '\\0' x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxOutput+len(truncMark) {
		t.Fatalf("output %d bytes, want truncated", len(out))
	}
	if _, err := (ExecRunner{}).Run(context.Background(), t.TempDir(), []string{"false"}); err == nil {
		t.Fatal("false: want error")
	}
}
