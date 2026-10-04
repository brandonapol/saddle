package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// landUnrelated lands t1..tn, each touching only its own file, so automatic
// clustering puts every PR on base.
func landUnrelated(t *testing.T, a *App, n int) []store.Task {
	t.Helper()
	var out []store.Task
	for i := 1; i <= n; i++ {
		id := "t" + string(rune('0'+i))
		out = append(out, landTask(t, a, id, id+" work", map[string]string{id + ".txt": id + "\n"}))
	}
	return out
}

func TestCreateStackRecordsOrderInStacksJSON(t *testing.T) {
	a := trainSetup(t)
	originWithGh(t, a)
	ts := landUnrelated(t, a, 3)
	// Refs by task id, by branch and by PR number all name tasks.
	must(t, a.Store.SetField(ts[0].ID, "pr", "https://github.com/o/r/pull/41"))
	rep, err := a.CreateStack("ui", []string{ts[2].ID, "41", ts[1].Branch})
	must(t, err)
	if !slices.Equal(rep.Stack.Tasks, []string{"t3", "t1", "t2"}) {
		t.Fatalf("stack tasks = %v, want t3 t1 t2 as given", rep.Stack.Tasks)
	}
	b, err := os.ReadFile(filepath.Join(a.Root, ".saddle", "stacks.json"))
	must(t, err)
	var f struct {
		Stacks []CustomStack `json:"stacks"`
	}
	must(t, json.Unmarshal(b, &f))
	if len(f.Stacks) != 1 || f.Stacks[0].Name != "ui" || !slices.Equal(f.Stacks[0].Tasks, []string{"t3", "t1", "t2"}) {
		t.Fatalf("stacks.json = %s", b)
	}
	got, err := a.CustomStacks()
	must(t, err)
	if len(got) != 1 || got[0].Name != "ui" {
		t.Fatalf("CustomStacks = %+v", got)
	}
}

func TestCreateStackValidates(t *testing.T) {
	a := trainSetup(t)
	originWithGh(t, a)
	landUnrelated(t, a, 3)
	queueTask(t, a, "t9", "queued only", map[string]string{"t9.txt": "x\n"})
	_, err := a.CreateStack("a", []string{"t1", "t2"})
	must(t, err)
	for _, c := range []struct {
		name string
		refs []string
		want string
	}{
		{"b", []string{"t3", "nope"}, `no task, PR or branch "nope"`},
		{"b", []string{"t3", "t9"}, "t9 hasn't landed"},
		{"b", []string{"t3", "t1"}, "t1 is already in stack a"},
		{"b", []string{"t3", "t3"}, "t3 is listed twice"},
		{"b", []string{"t3"}, "at least two tasks"},
		{"a", []string{"t3", "t9"}, "stack a already exists"},
		{"bad name", []string{"t3", "t9"}, "stack name"},
	} {
		_, err := a.CreateStack(c.name, c.refs)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("create %s %v: err = %v, want %q", c.name, c.refs, err, c.want)
		}
	}
	got, err := a.CustomStacks()
	must(t, err)
	if len(got) != 1 {
		t.Fatalf("a refused create was recorded: %+v", got)
	}
}

func TestStackArgsTakeOptionalName(t *testing.T) {
	a := trainSetup(t)
	originWithGh(t, a)
	landUnrelated(t, a, 2)
	name, refs := a.StackArgs([]string{"t1", "t2"})
	if name != "" || !slices.Equal(refs, []string{"t1", "t2"}) {
		t.Fatalf("no name: %q %v", name, refs)
	}
	name, refs = a.StackArgs([]string{"ui", "t1", "t2"})
	if name != "ui" || !slices.Equal(refs, []string{"t1", "t2"}) {
		t.Fatalf("named: %q %v", name, refs)
	}
	rep, err := a.CreateStack("", []string{"t2", "t1"})
	must(t, err)
	if rep.Stack.Name != "t2-stack" {
		t.Fatalf("default name = %q, want t2-stack (its bottom task)", rep.Stack.Name)
	}
}

// A custom stack chains its PRs bottom to top as given, even when the tasks
// share no files and land in another order; tasks outside it keep their
// automatic layout.
func TestCustomStackOverridesClustering(t *testing.T) {
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	ts := landUnrelated(t, a, 3)
	t1, t2, t3 := ts[0], ts[1], ts[2]
	rep, err := a.CreateStack("ui", []string{t3.ID, t1.ID})
	must(t, err)
	if rep.PublishErr != "" {
		t.Fatal(rep.PublishErr)
	}
	bases := prBases(ghLog())
	want := map[string]string{t3.Branch: "main", t1.Branch: t3.Branch, t2.Branch: "main"}
	for br, b := range want {
		if bases[br] != b {
			t.Fatalf("PR bases = %v, want %v", bases, want)
		}
	}
	// t1's PR head sits on t3's and carries only t1's own work above it.
	r1, r3 := remoteRev(t, origin, t1.Branch), remoteRev(t, origin, t3.Branch)
	if parent := git(t, origin, "rev-parse", r1+"^"); parent != r3 {
		t.Fatalf("t1's PR head is not on t3's: parent %s, t3 %s", parent, r3)
	}
	if files := git(t, origin, "ls-tree", "-r", "--name-only", r1); strings.Contains(files, "t2.txt") {
		t.Fatalf("t1's PR carries t2's work:\n%s", files)
	}
	// t3 pushed before t1, so t1's PR could target it.
	var pushed []string
	for _, c := range ghLog() {
		if strings.HasPrefix(c, "pr create") {
			pushed = append(pushed, c)
		}
	}
	if !strings.Contains(pushed[0], t3.Branch) && !strings.Contains(pushed[1], t3.Branch) {
		t.Fatalf("t3's PR wasn't opened before t1's: %q", pushed)
	}
	// The PR body lists the stack in its order.
	body := lastBody(t, mustTask(t, a, t1.ID).PR)
	i3, i1 := strings.Index(body, "t3 work"), strings.Index(body, "t1 work")
	if i3 < 0 || i1 < 0 || !strings.Contains(body, "**Stack**") {
		t.Fatalf("t1's body doesn't list the stack:\n%s", body)
	}
	// Numbered bottom first, printed top first: t1 (2.) before t3 (1.).
	if i1 > i3 || !strings.Contains(body, "2. ") {
		t.Fatalf("stack listed out of order:\n%s", body)
	}
	// Publishing again is stable.
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if got := remoteRev(t, origin, t1.Branch); got != r1 {
		t.Fatalf("t1 republished as %s, was %s", got, r1)
	}
}

// Members of a custom stack leave automatic clustering: two tasks for the
// same issue would stack, but one is in a custom stack, so the other goes
// to base.
func TestCustomStackMembersLeaveAutomaticClusters(t *testing.T) {
	a := trainSetup(t)
	_, ghLog := originWithGh(t, a)
	land := func(id string, issue int) store.Task {
		tk, err := a.Spawn(SpawnReq{ID: id, Title: id, Issue: issue})
		must(t, err)
		write(t, tk.Worktree, id+".txt", id+"\n")
		commitAll(t, tk.Worktree, id)
		must(t, a.Done(tk.ID, id))
		_, err = a.Land()
		must(t, err)
		return tk
	}
	t1, t2, t3 := land("t1", 52), land("t2", 52), land("t3", 0)
	_, err := a.CreateStack("mine", []string{t1.ID, t3.ID})
	must(t, err)
	bases := prBases(ghLog())
	if bases[t2.Branch] != "main" || bases[t3.Branch] != t1.Branch {
		t.Fatalf("bases = %v, want t2 on main and t3 on t1", bases)
	}
}

// When the given order can't be replayed (a lower task needs work landed
// after it), the stack keeps its members but falls back to train order.
func TestCustomStackOrderFallsBackOnConflict(t *testing.T) {
	a := trainSetup(t)
	_, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "one more", map[string]string{"one.txt": "one\nmore\n"})
	_, err := a.CreateStack("s", []string{t2.ID, t1.ID})
	must(t, err)
	bases := prBases(ghLog())
	if bases[t1.Branch] != "main" || bases[t2.Branch] != t1.Branch {
		t.Fatalf("bases = %v, want train order t1 then t2", bases)
	}
}

func TestRestackKeepsCustomStack(t *testing.T) {
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	ts := landUnrelated(t, a, 3)
	t1, t2, t3 := ts[0], ts[1], ts[2]
	_, err := a.CreateStack("ui", []string{t3.ID, t1.ID})
	must(t, err)
	before := len(ghLog())

	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "main.txt", "main\n")
	commitAll(t, other, "main moves")
	git(t, other, "push", "-q", "origin", "main")

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	edits := map[string]string{}
	for _, c := range ghLog()[before:] {
		if f := strings.Fields(c); len(f) == 5 && f[0] == "pr" && f[1] == "edit" && f[3] == "--base" {
			edits[f[2]] = f[4]
		}
	}
	want := map[string]string{t1.ID: t3.Branch, t2.ID: "main", t3.ID: "main"}
	for id, b := range want {
		if pr := mustTask(t, a, id).PR; edits[pr] != b {
			t.Fatalf("retargets = %v; %s want %s", edits, id, b)
		}
	}
	r1, r3 := remoteRev(t, origin, t1.Branch), remoteRev(t, origin, t3.Branch)
	if parent := git(t, origin, "rev-parse", r1+"^"); parent != r3 {
		t.Fatalf("after restack t1's PR head is not on t3's")
	}
}

func TestStackAddRemoveDelete(t *testing.T) {
	a := trainSetup(t)
	_, ghLog := originWithGh(t, a)
	ts := landUnrelated(t, a, 3)
	_, err := a.CreateStack("ui", []string{"t1", "t2"})
	must(t, err)
	rep, err := a.AddToStack("ui", []string{"t3"})
	must(t, err)
	if !slices.Equal(rep.Stack.Tasks, []string{"t1", "t2", "t3"}) {
		t.Fatalf("after add: %v", rep.Stack.Tasks)
	}
	if log := strings.Join(ghLog(), "\n"); !strings.Contains(log, "pr edit "+mustTask(t, a, "t3").PR+" --base "+ts[1].Branch) {
		t.Fatalf("t3 wasn't retargeted onto t2 after add:\n%s", log)
	}
	if _, err := a.AddToStack("ui", []string{"t3"}); err == nil {
		t.Fatal("adding a member twice should fail")
	}
	rep, err = a.RemoveFromStack("ui", []string{"t2"})
	must(t, err)
	if !slices.Equal(rep.Stack.Tasks, []string{"t1", "t3"}) {
		t.Fatalf("after remove: %v", rep.Stack.Tasks)
	}
	if !strings.Contains(strings.Join(ghLog(), "\n"), "pr edit "+mustTask(t, a, "t2").PR+" --base main") {
		t.Fatalf("t2 wasn't retargeted to main after leaving the stack:\n%s", strings.Join(ghLog(), "\n"))
	}
	if _, err := a.RemoveFromStack("ui", []string{"t2"}); err == nil {
		t.Fatal("removing a non-member should fail")
	}
	_, err = a.DeleteStack("ui")
	must(t, err)
	got, err := a.CustomStacks()
	must(t, err)
	if len(got) != 0 {
		t.Fatalf("after delete: %+v", got)
	}
	if _, err := a.DeleteStack("ui"); err == nil {
		t.Fatal("deleting a missing stack should fail")
	}
}

func TestShowStackReportsMembers(t *testing.T) {
	a := trainSetup(t)
	originWithGh(t, a)
	landUnrelated(t, a, 2)
	_, err := a.CreateStack("ui", []string{"t2", "t1"})
	must(t, err)
	v, err := a.ShowStack("t1") // a member names its stack
	must(t, err)
	if v.Name != "ui" || len(v.Members) != 2 || v.Members[0].Task != "t2" || v.Members[0].PR == "" || v.Members[1].Base != mustTask(t, a, "t2").Branch {
		t.Fatalf("show = %+v", v)
	}
}

func mustTask(t *testing.T, a *App, id string) store.Task {
	t.Helper()
	tk, err := a.Store.Task(id)
	must(t, err)
	return tk
}

// lastBody is the last --body the fake gh got for pr. Bodies span lines, so
// it reads the raw call log.
func lastBody(t *testing.T, pr string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv(fakeGHEnv), "gh.log"))
	must(t, err)
	log := string(b)
	i := strings.LastIndex(log, "pr edit "+pr+" --body ")
	if i < 0 {
		return ""
	}
	body := log[i:]
	if j := strings.Index(body, "\nBase: `"); j >= 0 {
		body = body[:j]
	}
	return body
}

