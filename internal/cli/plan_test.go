package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/planner"
)

func TestTicketIssueKeepsSubIssueBodies(t *testing.T) {
	got := ticketIssue(app.Ticket{Number: 6, Title: "Planner", State: "OPEN", Body: "b", URL: "u",
		SubIssues: []app.SubTicket{{Number: 7, Title: "Sources", State: "closed", Body: "s"}}})
	want := planner.Issue{Number: 6, Title: "Planner", State: "OPEN", Body: "b", URL: "u",
		Subs: []planner.Issue{{Number: 7, Title: "Sources", State: "closed", Body: "s"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ticketIssue = %+v, want %+v", got, want)
	}
}

const planJSON = `{"tasks":[
 {"id":"store","title":"Add table","goal":"Persist meters.","claims":["internal/store/**"],"deps":[],"barrier":false,"done_when":["tests pass"]},
 {"id":"cli","title":"Wire CLI","goal":"Add the command.","claims":["internal/cli/**"],"deps":["store"],"barrier":false,"done_when":["saddle x works"]}
]}`

type recordingModel struct {
	mu    sync.Mutex
	calls []planner.Call
}

func (m *recordingModel) Complete(_ context.Context, c planner.Call) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, c)
	return []byte(planJSON), nil
}

// planRepo makes a git repo with a commit on main, chdirs into it and swaps
// the planner model for a recording fake.
func planRepo(t *testing.T) (string, *recordingModel) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "epics"), 0o755))
	must(os.WriteFile(filepath.Join(root, "epics", "foo.md"), []byte("# Billing rewrite\n\nSplit meters.\n"), 0o644))
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "init")
	t.Chdir(root)
	m := &recordingModel{}
	old := planModel
	planModel = func(*app.App, string) planner.Model { return m }
	t.Cleanup(func() { planModel = old })
	return root, m
}

func runPlan(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	cmd := Root()
	cmd.SetArgs(args)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}

func TestPlanReviewFlow(t *testing.T) {
	root, m := planRepo(t)
	path := filepath.Join(root, ".saddle", "plans", "billing-rewrite.toml")

	out, err := runPlan(t, "", "plan", "epics/foo.md")
	if err != nil {
		t.Fatalf("plan: %v\n%s", err, out)
	}
	if !strings.Contains(out, path) || !strings.Contains(out, "wave 2: cli") {
		t.Fatalf("plan output:\n%s", out)
	}
	if len(m.calls) != 1 || !strings.Contains(m.calls[0].User, "Split meters.") {
		t.Fatalf("model calls = %+v", m.calls)
	}

	// The editor renames a task; the checker re-runs and the file is re-rendered.
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", `sed -i 's/title = "Wire CLI"/title = "Wire the CLI"/'`)
	if out, err := runPlan(t, "", "plan", "edit", path); err != nil || !strings.Contains(out, "wave 2: cli") {
		t.Fatalf("plan edit: %v\n%s", err, out)
	}
	d, err := planner.LoadDoc(path)
	if err != nil || d.Tasks[1].Title != "Wire the CLI" {
		t.Fatalf("after edit: %+v, %v", d, err)
	}

	// A bad edit is kept for the next round and reported.
	t.Setenv("EDITOR", `sed -i 's/deps = \["store"\]/deps = ["nope"]/'`)
	if out, err := runPlan(t, "", "plan", "edit", path); err == nil || !strings.Contains(err.Error()+out, `unknown task "nope"`) {
		t.Fatalf("bad edit: %v\n%s", err, out)
	}
	t.Setenv("EDITOR", `sed -i 's/deps = \["nope"\]/deps = ["store"]/'`)
	if _, err := runPlan(t, "", "plan", "edit", path); err != nil {
		t.Fatal(err)
	}

	out, err = runPlan(t, "", "plan", "approve", path)
	if err != nil {
		t.Fatalf("approve: %v\n%s", err, out)
	}
	head := strings.TrimSpace(gitRun(t, root, "rev-parse", "main"))
	if d, _ := planner.LoadDoc(path); !d.Approved || d.Base != head {
		t.Fatalf("approved doc = %+v, want base %s", d, head)
	}
	if _, err := runPlan(t, "", "plan", "edit", path); !errors.Is(err, planner.ErrApproved) {
		t.Fatalf("edit of frozen plan: %v", err)
	}
	if _, err := runPlan(t, "", "plan", "reopen", path); err != nil {
		t.Fatal(err)
	}
	if out, err := runPlan(t, "", "plan", "show", path); err != nil || !strings.Contains(out, "approved = false") {
		t.Fatalf("show: %v\n%s", err, out)
	}
}

func TestPlanFromStdinAndOutFlag(t *testing.T) {
	root, m := planRepo(t)
	out := filepath.Join(root, "my-plan.toml")
	if o, err := runPlan(t, "# Piped epic\nbody text\n", "plan", "-", "-o", out); err != nil {
		t.Fatalf("%v\n%s", err, o)
	}
	d, err := planner.LoadDoc(out)
	if err != nil || d.Epic != "Piped epic" || d.Source != "-" {
		t.Fatalf("doc = %+v, %v", d, err)
	}
	if !strings.Contains(m.calls[0].User, "body text") {
		t.Fatalf("prompt lacks stdin epic:\n%s", m.calls[0].User)
	}
}

func TestPlanRefusesToOverwrite(t *testing.T) {
	_, _ = planRepo(t)
	if _, err := runPlan(t, "", "plan", "epics/foo.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runPlan(t, "", "plan", "epics/foo.md"); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("second plan: %v", err)
	}
	if _, err := runPlan(t, "", "plan", "epics/foo.md", "--force"); err != nil {
		t.Fatalf("--force: %v", err)
	}
}

func TestPlanReplanSendsNoteAndPreviousPlan(t *testing.T) {
	root, m := planRepo(t)
	path := filepath.Join(root, ".saddle", "plans", "billing-rewrite.toml")
	if _, err := runPlan(t, "", "plan", "epics/foo.md"); err != nil {
		t.Fatal(err)
	}
	if out, err := runPlan(t, "", "plan", "replan", path, "--note", "merge store into cli"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	c := m.calls[1].User
	for _, want := range []string{"Split meters.", "Previous plan", `"id": "store"`, "merge store into cli"} {
		if !strings.Contains(c, want) {
			t.Errorf("replan prompt lacks %q:\n%s", want, c)
		}
	}
	if _, err := runPlan(t, "", "plan", "replan", path); err == nil {
		t.Error("replan without a note should fail")
	}
}

func TestPlanFromGitHubIssue(t *testing.T) {
	root, m := planRepo(t)
	bin := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
"issue view") echo '{"number":12,"title":"Planner epic","state":"OPEN","body":"Top body.","url":"u","labels":[]}' ;;
"api "*) echo '[{"number":13,"title":"Sources","state":"open","body":"Sub body."}]' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := runPlan(t, "", "plan", "gh:#12"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	d, err := planner.LoadDoc(filepath.Join(root, ".saddle", "plans", "planner-epic.toml"))
	if err != nil || d.Issue != 12 || d.Source != "gh:#12" {
		t.Fatalf("doc = %+v, %v", d, err)
	}
	for _, want := range []string{"Top body.", "#13 Sources", "Sub body."} {
		if !strings.Contains(m.calls[0].User, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
}

func TestPlanPushCreatesIssues(t *testing.T) {
	root, _ := planRepo(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "gh.log")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1 $2" in
"issue create") n=$(grep -c '^issue create' "` + log + `"); echo "https://github.com/o/r/issues/$((40+n))" ;;
"api "*) case "$*" in *POST*|*sub_issues*) ;; *) echo 777 ;; esac ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(root, ".saddle", "plans", "billing-rewrite.toml")
	if _, err := runPlan(t, "", "plan", "epics/foo.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := runPlan(t, "", "plan", "push", path); !errors.Is(err, planner.ErrNotApproved) {
		t.Fatalf("push before approve: %v", err)
	}
	if _, err := runPlan(t, "", "plan", "approve", path); err != nil {
		t.Fatal(err)
	}
	out, err := runPlan(t, "", "plan", "push", path)
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	for _, want := range []string{"epic #41", "store #42", "cli #43"} {
		if !strings.Contains(out, want) {
			t.Errorf("push output lacks %q:\n%s", want, out)
		}
	}
	d, _ := planner.LoadDoc(path)
	if d.Issue != 41 || d.Tasks[1].Issues[0] != "#43" {
		t.Fatalf("doc = %+v", d)
	}
}
