package planner

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sampleDoc() Doc {
	return Doc{
		Epic:   "Billing rewrite",
		Source: "gh:#12",
		Issue:  12,
		Text:   "# Billing rewrite\n\nSplit meters.\n",
		Tasks: []Task{
			{ID: "store", Title: "Add table", Plan: "Persist meters.\nKeep it small.", Claims: []string{"internal/store/**"}, DoneWhen: []string{"tests pass"}, Model: "opus"},
			{ID: "cli", Title: "Wire CLI", Plan: "Add the \"plan\" command.\nPaths like C:\\x\tand \"\"\" survive.\n", Claims: []string{"internal/cli/**", "go.mod"}, After: []string{"store"}, DoneWhen: []string{"saddle x works"}, Barrier: false},
		},
	}
}

func TestRenderRoundTrips(t *testing.T) {
	d := sampleDoc()
	out := Render(d, []string{"go.mod"}, 0)
	for _, want := range []string{"[[task]]", `id = "store"`, "Keep it small.", "# wave 1: store", "# wave 2: cli", "# train: cli"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	got, err := Decode(out)
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(got, d) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, d)
	}
}

func TestRenderShowsCheckErrors(t *testing.T) {
	d := sampleDoc()
	d.Tasks[0].After = []string{"cli"}
	out := string(Render(d, nil, 0))
	if !strings.Contains(out, "# check failed: planner: dependency cycle") {
		t.Fatalf("render lacks the cycle:\n%s", out)
	}
}

func TestDecodeIsStrict(t *testing.T) {
	good := string(Render(sampleDoc(), nil, 0))
	for name, in := range map[string]string{
		"unknown key":  good + "\n[[task]]\nid = \"x\"\nowner = \"me\"\n",
		"bad toml":     good + "\n[[task]\n",
		"invalid task": strings.Replace(good, `title = "Add table"`, `title = ""`, 1),
		"no tasks":     "epic = \"x\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(in)); err == nil {
				t.Fatalf("Decode accepted:\n%s", in)
			}
		})
	}
}

func writeDoc(t *testing.T, d Doc) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "plan.toml")
	if err := WriteDoc(p, d, nil, 0); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEditRechecks(t *testing.T) {
	p := writeDoc(t, sampleDoc())
	editor := func(path string) error {
		b, _ := os.ReadFile(path)
		s := strings.Replace(string(b), `title = "Wire CLI"`, `title = "Wire the CLI"`, 1)
		return os.WriteFile(path, []byte(s), 0o644)
	}
	d, plan, err := Edit(p, editor, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tasks[1].Title != "Wire the CLI" || len(plan.Waves) != 2 {
		t.Fatalf("doc=%+v plan=%+v", d.Tasks[1], plan)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "# wave 2: cli") {
		t.Errorf("edited file was not re-rendered with the check:\n%s", b)
	}
}

func TestEditKeepsBrokenFileAndReportsWhy(t *testing.T) {
	p := writeDoc(t, sampleDoc())
	editor := func(path string) error {
		b, _ := os.ReadFile(path)
		s := strings.Replace(string(b), `deps = ["store"]`, `deps = ["nope"]`, 1)
		return os.WriteFile(path, []byte(s), 0o644)
	}
	_, _, err := Edit(p, editor, nil, 0)
	if err == nil || !strings.Contains(err.Error(), `unknown task "nope"`) {
		t.Fatalf("err = %v", err)
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `deps = ["nope"]`) {
		t.Error("the user's edit was lost")
	}
}

func TestEditRejectsCycle(t *testing.T) {
	p := writeDoc(t, sampleDoc())
	editor := func(path string) error {
		b, _ := os.ReadFile(path)
		s := strings.Replace(string(b), `id = "store"`, "id = \"store\"\ndeps = [\"cli\"]", 1)
		return os.WriteFile(path, []byte(s), 0o644)
	}
	var ce *CycleError
	if _, _, err := Edit(p, editor, nil, 0); !errors.As(err, &ce) {
		t.Fatalf("err = %v, want CycleError", err)
	}
}

func TestEditorFailureLeavesPlan(t *testing.T) {
	p := writeDoc(t, sampleDoc())
	before, _ := os.ReadFile(p)
	if _, _, err := Edit(p, func(string) error { return errors.New("vi crashed") }, nil, 0); err == nil {
		t.Fatal("want error")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Error("plan changed although the editor failed")
	}
}

func TestApproveFreezesBase(t *testing.T) {
	p := writeDoc(t, sampleDoc())
	d, err := Approve(p, "abc123", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Approved || d.Base != "abc123" {
		t.Fatalf("doc = %+v", d)
	}
	got, err := LoadDoc(p)
	if err != nil || !got.Approved || got.Base != "abc123" {
		t.Fatalf("reloaded = %+v, %v", got, err)
	}
	// A frozen plan can't be edited or re-approved until it is reopened.
	if _, _, err := Edit(p, func(string) error { return nil }, nil, 0); !errors.Is(err, ErrApproved) {
		t.Fatalf("edit after approve: %v", err)
	}
	if _, err := Approve(p, "def456", nil, 0); !errors.Is(err, ErrApproved) {
		t.Fatalf("approve twice: %v", err)
	}
	d, err = Reopen(p, nil, 0)
	if err != nil || d.Approved || d.Base != "" {
		t.Fatalf("reopen = %+v, %v", d, err)
	}
	if _, _, err := Edit(p, func(string) error { return nil }, nil, 0); err != nil {
		t.Fatalf("edit after reopen: %v", err)
	}
}

func TestApproveRefusesBrokenPlan(t *testing.T) {
	d := sampleDoc()
	d.Tasks[0].After = []string{"cli"}
	p := writeDoc(t, d)
	if _, err := Approve(p, "abc", nil, 0); err == nil {
		t.Fatal("approved a plan with a cycle")
	}
	if got, _ := os.ReadFile(p); strings.Contains(string(got), "approved = true") {
		t.Fatal("broken plan was frozen")
	}
}

func TestEditReopensABrokenFileToFixIt(t *testing.T) {
	p := writeDoc(t, sampleDoc())
	breakIt := func(path string) error {
		b, _ := os.ReadFile(path)
		return os.WriteFile(path, []byte(strings.Replace(string(b), `deps = ["store"]`, `deps = ["nope"]`, 1)), 0o644)
	}
	if _, _, err := Edit(p, breakIt, nil, 0); err == nil {
		t.Fatal("want error")
	}
	fixIt := func(path string) error {
		b, _ := os.ReadFile(path)
		return os.WriteFile(path, []byte(strings.Replace(string(b), `deps = ["nope"]`, `deps = ["store"]`, 1)), 0o644)
	}
	if _, _, err := Edit(p, fixIt, nil, 0); err != nil {
		t.Fatalf("second edit could not fix the plan: %v", err)
	}
}
