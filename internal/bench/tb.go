package bench

import (
	"fmt"
	"io"
	"os"
	"testing"
)

// runT lets the bench drive internal/e2e's World outside `go test`. The
// World only needs a few testing.TB methods; the embedded nil TB satisfies
// the interface's unexported method and panics if anything else is called,
// which Run recovers as an error. Fatal panics with a fatal, which Run
// recovers the same way.
type runT struct {
	testing.TB
	dir      string
	out      io.Writer
	n        int
	failed   bool
	cleanups []func()
}

// fatal is a Fatal from inside the World.
type fatal struct{ msg string }

func newRunT(dir string, out io.Writer) *runT { return &runT{dir: dir, out: out} }

func (t *runT) Helper()      {}
func (t *runT) Name() string { return "bench" }
func (t *runT) Failed() bool { return t.failed }

func (t *runT) Fatal(args ...any) { t.failed = true; panic(fatal{fmt.Sprint(args...)}) }
func (t *runT) Fatalf(format string, args ...any) {
	t.failed = true
	panic(fatal{fmt.Sprintf(format, args...)})
}
func (t *runT) Error(args ...any)                 { t.failed = true; t.Log(args...) }
func (t *runT) Errorf(format string, args ...any) { t.failed = true; t.Logf(format, args...) }
func (t *runT) Log(args ...any)                   { fmt.Fprintln(t.out, args...) }
func (t *runT) Logf(format string, args ...any)   { fmt.Fprintf(t.out, format+"\n", args...) }

func (t *runT) Cleanup(f func()) { t.cleanups = append(t.cleanups, f) }

func (t *runT) TempDir() string {
	t.n++
	d := fmt.Sprintf("%s/tmp%d", t.dir, t.n)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// Setenv sets k for this process and restores it on close.
func (t *runT) Setenv(k, v string) {
	old, had := os.LookupEnv(k)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(k, old)
		} else {
			_ = os.Unsetenv(k)
		}
	})
	_ = os.Setenv(k, v)
}

// close runs the cleanups, newest first, as testing does.
func (t *runT) close() {
	for i := len(t.cleanups) - 1; i >= 0; i-- {
		func() {
			defer func() { _ = recover() }()
			t.cleanups[i]()
		}()
	}
	t.cleanups = nil
}
