package doctor

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/refguard"
	"github.com/brandonapol/saddle/internal/store"
)

// System is the real Env for the repo whose main checkout is root.
func System(root string) Env { return system(root) }

type system string

func (s system) Root() string                       { return string(s) }
func (s system) Config() (config.Config, error)     { return config.Load(string(s)) }
func (s system) Git(args ...string) (string, error) { return gitx.Run(string(s), args...) }
func (s system) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}
func (s system) Hooks() ([]refguard.HookState, error) { return refguard.Installed(string(s)) }

func (s system) GH(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = string(s)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (s system) Exec(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = string(s)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (s system) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (s system) OpenStore(path string) error {
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	return st.Close()
}

// Leftovers counts what saddle gc would remove and keep, the same way gc
// decides. An uninitialized repo has none.
func (s system) Leftovers() (remove, kept int, err error) {
	if _, err := os.Stat(filepath.Join(string(s), ".saddle", "state.db")); errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	a, err := app.Open(string(s))
	if err != nil {
		return 0, 0, err
	}
	defer a.Close()
	return a.GCCounts()
}
