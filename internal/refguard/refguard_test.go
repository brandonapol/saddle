package refguard

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// TestMain lets the test binary stand in for saddle: the installed hook runs
// `<bin> refguard <state>`.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "refguard" {
		if err := Hook(os.Args[2], os.Stdin, os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type repo struct {
	t    *testing.T
	root string
	st   *store.Store
}

func setup(t *testing.T) *repo {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	r := &repo{t: t, root: root}
	r.git("", "init", "-q", "-b", "main")
	r.git("", "commit", "-q", "--allow-empty", "-m", "one")
	r.git("", "commit", "-q", "--allow-empty", "-m", "two")
	st, err := store.Open(filepath.Join(root, ".saddle", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r.st = st
	for _, id := range []string{"t1", "t2"} {
		if err := st.CreateTask(store.Task{ID: id, Title: id, Status: store.Running}); err != nil {
			t.Fatal(err)
		}
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := Install(root, bin); err != nil {
		t.Fatal(err)
	}
	return r
}

// run runs git as actor: "train", a task id, or "" for nobody.
func (r *repo) run(actor string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", r.root}, args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SADDLE_") && !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	switch actor {
	case "":
	case "train":
		// The train runs inside the orchestrator's MCP server, so it carries a task id too.
		cmd.Env = append(cmd.Env, "SADDLE_TRAIN=1", "SADDLE_TASK=t0")
	default:
		cmd.Env = append(cmd.Env, "SADDLE_TASK="+actor)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func (r *repo) git(actor string, args ...string) {
	r.t.Helper()
	if err := r.run(actor, args...); err != nil {
		r.t.Fatalf("git %v as %q: %v", args, actor, err)
	}
}

func (r *repo) deny(actor string, args ...string) {
	r.t.Helper()
	if err := r.run(actor, args...); err == nil {
		r.t.Fatalf("git %v as %q: want denied, got success", args, actor)
	}
}

func (r *repo) rev(ref string) string {
	r.t.Helper()
	out, err := gitx.Run(r.root, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return out
}

type attempt struct {
	actor, ref string
	denied     bool
}

func (r *repo) attempts() []attempt {
	r.t.Helper()
	evs, err := r.st.Events(1000)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []attempt
	for _, e := range evs {
		if e.Kind != KindRef && e.Kind != KindDenied {
			continue
		}
		var u Event
		if err := json.Unmarshal([]byte(e.Data), &u); err != nil {
			r.t.Fatalf("event %q: %v", e.Data, err)
		}
		if u.Actor != e.Task {
			r.t.Errorf("event task %q, actor %q", e.Task, u.Actor)
		}
		if (e.Kind == KindDenied) != (u.Denied != "") {
			r.t.Errorf("event kind %s with denial %q", e.Kind, u.Denied)
		}
		out = append(out, attempt{u.Actor, u.Ref, e.Kind == KindDenied})
	}
	return out
}

func TestGuard(t *testing.T) {
	r := setup(t)
	const integ, t1 = "refs/heads/saddle/integration", "refs/heads/saddle/t1-x"

	// Creating saddle branches is how spawn starts a task; anyone may.
	r.git("t0", "branch", "saddle/integration", "HEAD~1")
	r.git("t0", "branch", "saddle/t1-x", "HEAD~1")
	two, one := r.rev("HEAD"), r.rev("HEAD~1")

	// A worker can't move the integration branch or another task's branch.
	r.deny("t2", "update-ref", integ, "HEAD")
	r.deny("t2", "branch", "-f", "saddle/t1-x", "HEAD")
	r.deny("", "update-ref", integ, "HEAD")
	r.deny("t1", "update-ref", integ, "HEAD")
	if r.rev(integ) != one || r.rev(t1) != one {
		t.Fatal("a denied update moved a ref")
	}

	// A task moves its own branch; the train moves anything.
	r.git("t1", "branch", "-f", "saddle/t1-x", "HEAD")
	r.git("train", "update-ref", integ, "HEAD")
	r.git("train", "branch", "-f", "saddle/t1-x", "HEAD~1")
	if r.rev(integ) != two || r.rev(t1) != one {
		t.Fatal("an allowed update did not move its ref")
	}

	// A live task's branch can't be deleted, even by itself.
	r.deny("t1", "branch", "-D", "saddle/t1-x")
	r.deny("t2", "update-ref", "-d", t1)
	if err := r.st.SetStatus("t1", store.Landed); err != nil {
		t.Fatal(err)
	}
	r.git("t2", "branch", "-D", "saddle/t1-x")
	if r.rev(t1) != "" {
		t.Fatal("deleting a landed task's branch did not delete it")
	}

	// Other refs are not saddle's business.
	r.git("t2", "commit", "-q", "--allow-empty", "-m", "three")

	want := []attempt{
		{"t0", integ, false}, {"t0", t1, false},
		{"t2", integ, true}, {"t2", t1, true}, {"unknown", integ, true}, {"t1", integ, true},
		{"t1", t1, false}, {"train", integ, false}, {"train", t1, false},
		{"t1", t1, true}, {"t2", t1, true}, {"t2", t1, false},
	}
	got := r.attempts()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events:\n got %v\nwant %v", got, want)
	}
}

func TestEventRecordsCurrentOld(t *testing.T) {
	r := setup(t)
	r.git("t1", "branch", "saddle/t1-x", "HEAD~1")
	one := r.rev("HEAD~1")
	// git passes a zero old value when the caller didn't pin one.
	r.deny("t2", "branch", "-f", "saddle/t1-x", "HEAD")
	evs, err := r.st.Events(1)
	if err != nil || len(evs) != 1 {
		t.Fatal(evs, err)
	}
	var u Event
	if err := json.Unmarshal([]byte(evs[0].Data), &u); err != nil {
		t.Fatal(err)
	}
	if u.Old != one || u.New != r.rev("HEAD") {
		t.Fatalf("event %+v: want old %s", u, one)
	}
}

func TestInstallKeepsForeignHook(t *testing.T) {
	r := setup(t)
	hooks, err := gitx.Run(r.root, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(hooks, "reference-transaction")
	// Reinstalling over our own hook is fine.
	if err := Install(r.root, "/bin/true"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Install(r.root, "/bin/true"); err == nil {
		t.Fatal("Install overwrote a hook it did not write")
	}
}
