package planner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const validJSON = `{"tasks":[
 {"id":"store","title":"Add epics table","goal":"Persist epics.","claims":["internal/store/**"],"deps":[],"model":"opus","adapter":"claude","barrier":false,"done_when":["go test ./internal/store passes"]},
 {"id":"cli","title":"Wire saddle plan","goal":"CLI command.","claims":["internal/cli/plan.go"],"deps":["store"],"done_when":["saddle plan --help works"]}
]}`

func TestParse(t *testing.T) {
	tasks, err := Parse([]byte(validJSON))
	if err != nil {
		t.Fatal(err)
	}
	want := []Task{
		{ID: "store", Title: "Add epics table", Plan: "Persist epics.", Claims: []string{"internal/store/**"}, Model: "opus", Adapter: "claude", DoneWhen: []string{"go test ./internal/store passes"}},
		{ID: "cli", Title: "Wire saddle plan", Plan: "CLI command.", Claims: []string{"internal/cli/plan.go"}, After: []string{"store"}, DoneWhen: []string{"saddle plan --help works"}},
	}
	if !reflect.DeepEqual(tasks, want) {
		t.Fatalf("Parse:\n got %+v\nwant %+v", tasks, want)
	}
}

func TestParseRejects(t *testing.T) {
	one := func(fields string) string {
		return `{"tasks":[{` + fields + `}]}`
	}
	base := `"id":"a","title":"T","goal":"G","claims":["pkg/**"],"done_when":["tests pass"]`
	tests := []struct {
		name, in, want string
	}{
		{"not json", `{"tasks":`, "parse plan"},
		{"unknown top-level field", `{"tasks":[],"extra":1}`, "unknown field"},
		{"unknown task field", one(base + `,"owner":"me"`), "unknown field"},
		{"no tasks", `{"tasks":[]}`, "no tasks"},
		{"missing id", one(`"title":"T","goal":"G","claims":["pkg/**"],"done_when":["x"]`), "task 0: id is required"},
		{"bad id", one(`"id":"Bad Id","title":"T","goal":"G","claims":["pkg/**"],"done_when":["x"]`), "lowercase"},
		{"missing title", one(`"id":"a","goal":"G","claims":["pkg/**"],"done_when":["x"]`), `task "a": title is required`},
		{"missing goal", one(`"id":"a","title":"T","claims":["pkg/**"],"done_when":["x"]`), `task "a": goal is required`},
		{"no claims", one(`"id":"a","title":"T","goal":"G","done_when":["x"]`), `task "a": claims`},
		{"absolute claim", one(`"id":"a","title":"T","goal":"G","claims":["/etc/**"],"done_when":["x"]`), "repo-relative"},
		{"escaping claim", one(`"id":"a","title":"T","goal":"G","claims":["../x"],"done_when":["x"]`), "repo-relative"},
		{"no done_when", one(`"id":"a","title":"T","goal":"G","claims":["pkg/**"]`), `task "a": done_when`},
		{"duplicate id", `{"tasks":[{` + base + `},{` + base + `}]}`, "duplicate task id"},
		{"unknown dep", one(base + `,"deps":["zzz"]`), "unknown task"},
		{"self dep", one(base + `,"deps":["a"]`), "itself"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.in))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	_, err := Parse([]byte(`{"tasks":[{"id":"a"},{"id":"b","title":"T"}]}`))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{`task "a": title`, `task "b": goal`, `task "b": claims`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

func TestSchemaMatchesTaskFields(t *testing.T) {
	var s struct {
		Type                 string   `json:"type"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
		Properties           struct {
			Tasks struct {
				Items struct {
					Required             []string                   `json:"required"`
					AdditionalProperties bool                       `json:"additionalProperties"`
					Properties           map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"tasks"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(Schema(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Type != "object" || s.AdditionalProperties || !slices.Equal(s.Required, []string{"tasks"}) {
		t.Fatalf("top level = %+v", s)
	}
	items := s.Properties.Tasks.Items
	var props []string
	for k := range items.Properties {
		props = append(props, k)
	}
	slices.Sort(props)
	want := []string{"adapter", "barrier", "claims", "deps", "done_when", "goal", "id", "model", "title"}
	if !slices.Equal(props, want) {
		t.Fatalf("task properties = %v, want %v", props, want)
	}
	if items.AdditionalProperties {
		t.Fatal("task schema must forbid additional properties")
	}
	for _, r := range []string{"id", "title", "goal", "claims", "done_when"} {
		if !slices.Contains(items.Required, r) {
			t.Errorf("%s not required", r)
		}
	}
}

func TestBuildCall(t *testing.T) {
	req := Request{
		Epic: "# Planner\nBuild the planner.",
		Snapshot: Snapshot{
			Tree:       []string{"cmd/", "internal/app/ (12 files)"},
			Codeowners: "* @brandon",
			Churn:      []Churn{{Path: "internal/app/train.go", Commits: 9}},
			Serial:     []string{"go.mod", "go.sum"},
		},
	}
	c := BuildCall(req)
	for _, want := range []string{"Build the planner.", "internal/app/ (12 files)", "* @brandon", "internal/app/train.go (9 commits)", "go.mod", "go.sum"} {
		if !strings.Contains(c.User, want) {
			t.Errorf("user prompt lacks %q:\n%s", want, c.User)
		}
	}
	if !strings.Contains(c.System, "claims") || string(c.Schema) != string(Schema()) {
		t.Errorf("system prompt or schema missing")
	}
	if strings.Contains(c.User, "Previous plan") {
		t.Error("first plan should not mention a previous plan")
	}

	req.Previous = []Task{{ID: "x", Title: "X", Plan: "do x", Claims: []string{"x/**"}, DoneWhen: []string{"ok"}}}
	req.Note = "split x in two"
	c = BuildCall(req)
	for _, want := range []string{"Previous plan", `"id": "x"`, "split x in two"} {
		if !strings.Contains(c.User, want) {
			t.Errorf("re-plan prompt lacks %q:\n%s", want, c.User)
		}
	}
}

type fakeModel struct {
	replies [][]byte
	calls   []Call
}

func (f *fakeModel) Complete(_ context.Context, c Call) ([]byte, error) {
	f.calls = append(f.calls, c)
	if len(f.replies) == 0 {
		return nil, errors.New("no more replies")
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

func TestGenerate(t *testing.T) {
	m := &fakeModel{replies: [][]byte{[]byte(validJSON)}}
	d, err := Generate(context.Background(), m, Request{Epic: "e", Snapshot: Snapshot{Serial: []string{"go.mod"}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Tasks) != 2 || !reflect.DeepEqual(d.Check.Waves, [][]string{{"store"}, {"cli"}}) {
		t.Fatalf("draft = %+v", d)
	}
	if len(m.calls) != 1 {
		t.Fatalf("calls = %d", len(m.calls))
	}
}

func TestGenerateRetriesOnceWithTheError(t *testing.T) {
	m := &fakeModel{replies: [][]byte{[]byte(`{"tasks":[{"id":"a"}]}`), []byte(validJSON)}}
	d, err := Generate(context.Background(), m, Request{Epic: "e"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Tasks) != 2 || len(m.calls) != 2 {
		t.Fatalf("tasks=%d calls=%d", len(d.Tasks), len(m.calls))
	}
	if !strings.Contains(m.calls[1].User, `task "a": title is required`) {
		t.Errorf("retry prompt lacks the validation error:\n%s", m.calls[1].User)
	}
}

func TestGenerateGivesUpAfterTwoInvalidPlans(t *testing.T) {
	bad := []byte(`{"tasks":[]}`)
	m := &fakeModel{replies: [][]byte{bad, bad, []byte(validJSON)}}
	if _, err := Generate(context.Background(), m, Request{Epic: "e"}, 0); err == nil || !strings.Contains(err.Error(), "no tasks") {
		t.Fatalf("err = %v", err)
	}
	if len(m.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(m.calls))
	}
}

func TestGenerateRejectsCycle(t *testing.T) {
	cyc := `{"tasks":[
 {"id":"a","title":"A","goal":"g","claims":["a/**"],"deps":["b"],"done_when":["x"]},
 {"id":"b","title":"B","goal":"g","claims":["b/**"],"deps":["a"],"done_when":["x"]}]}`
	m := &fakeModel{replies: [][]byte{[]byte(cyc), []byte(cyc)}}
	var ce *CycleError
	if _, err := Generate(context.Background(), m, Request{Epic: "e"}, 0); !errors.As(err, &ce) {
		t.Fatalf("err = %v, want CycleError", err)
	}
}

func TestCheckHonorsExplicitBarrier(t *testing.T) {
	a := task("a", "a/**")
	a.Barrier = true
	p, err := Check([]Task{a, task("b", "b/**")}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(p.Barriers), []string{"a"}) || !reflect.DeepEqual(p.Waves, [][]string{{"a"}, {"b"}}) {
		t.Fatalf("barriers=%v waves=%v", p.Barriers, p.Waves)
	}
}

func TestAnthropicForcesPlanTool(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "bad headers", http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"stop_reason":"tool_use","content":[{"type":"text","text":"ok"},{"type":"tool_use","name":"submit_plan","input":`+validJSON+`}]}`)
	}))
	defer srv.Close()
	m := &Anthropic{APIKey: "k", Endpoint: srv.URL, Model: "claude-opus-5-5"}
	out, err := m.Complete(context.Background(), BuildCall(Request{Epic: "e"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(out); err != nil {
		t.Fatalf("returned input does not parse: %v", err)
	}
	if got["model"] != "claude-opus-5-5" {
		t.Errorf("model = %v", got["model"])
	}
	tc, _ := got["tool_choice"].(map[string]any)
	if tc["type"] != "tool" || tc["name"] != "submit_plan" {
		t.Errorf("tool_choice = %v", got["tool_choice"])
	}
}

func TestAnthropicErrors(t *testing.T) {
	for name, reply := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"http error": {500, `{"error":"boom"}`, "500"},
		"no tool":    {200, `{"stop_reason":"end_turn","content":[{"type":"text","text":"hi"}]}`, "no submit_plan"},
		"truncated":  {200, `{"stop_reason":"max_tokens","content":[]}`, "max_tokens"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(reply.status)
				_, _ = io.WriteString(w, reply.body)
			}))
			defer srv.Close()
			_, err := (&Anthropic{APIKey: "k", Endpoint: srv.URL}).Complete(context.Background(), BuildCall(Request{Epic: "e"}))
			if err == nil || !strings.Contains(err.Error(), reply.want) {
				t.Fatalf("err = %v, want %q", err, reply.want)
			}
		})
	}
}

func TestClaudeCLIExtractsJSON(t *testing.T) {
	var gotArgs []string
	var gotStdin string
	m := &ClaudeCLI{Cmd: "claude", Model: "opus", Run: func(_ context.Context, name string, args []string, stdin string) ([]byte, error) {
		gotArgs = append([]string{name}, args...)
		gotStdin = stdin
		res, _ := json.Marshal(map[string]any{"type": "result", "is_error": false, "result": "Here is the plan:\n```json\n" + validJSON + "\n```"})
		return res, nil
	}}
	out, err := m.Complete(context.Background(), BuildCall(Request{Epic: "the epic"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(out); err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if !slices.Contains(gotArgs, "-p") || !slices.Contains(gotArgs, "opus") {
		t.Errorf("args = %v", gotArgs)
	}
	if !strings.Contains(gotStdin, "the epic") || !strings.Contains(gotStdin, `"done_when"`) {
		t.Errorf("stdin lacks epic or schema:\n%s", gotStdin)
	}
}

func TestClaudeCLIReportsError(t *testing.T) {
	m := &ClaudeCLI{Run: func(context.Context, string, []string, string) ([]byte, error) {
		return []byte(`{"is_error":true,"result":"rate limited"}`), nil
	}}
	if _, err := m.Complete(context.Background(), Call{}); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v", err)
	}
}

func TestTakeSnapshot(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(p, s string) {
		t.Helper()
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("README.md", "r")
	write(".github/CODEOWNERS", "* @owner\n")
	write("internal/app/a.go", "a")
	write("internal/app/b.go", "b")
	write("internal/app/deep/c.go", "c")
	git("add", ".")
	git("commit", "-qm", "one")
	write("internal/app/a.go", "a2")
	git("commit", "-qam", "two")

	s, err := TakeSnapshot(context.Background(), dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	wantTree := []string{".github/CODEOWNERS", "README.md", "internal/app/ (3 files)"}
	if !slices.Equal(s.Tree, wantTree) {
		t.Errorf("tree = %q, want %q", s.Tree, wantTree)
	}
	if strings.TrimSpace(s.Codeowners) != "* @owner" {
		t.Errorf("codeowners = %q", s.Codeowners)
	}
	if len(s.Churn) == 0 || s.Churn[0] != (Churn{Path: "internal/app/a.go", Commits: 2}) {
		t.Errorf("churn = %+v", s.Churn)
	}
}
