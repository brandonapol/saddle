package remote

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

const bashPrompt = `● Bash(go test ./...)
  ⎿  Running…

╭──────────────────────────────────────────────────────────────╮
│ Bash command                                                 │
│                                                              │
│   go test ./internal/remote/...                              │
│   Run the remote tests                                       │
│                                                              │
│ Do you want to proceed?                                      │
│ ❯ 1. Yes                                                     │
│   2. Yes, and don't ask again for go test commands in        │
│      /home/me/src/saddle                                     │
│   3. No, and tell Claude what to do differently (esc)        │
╰──────────────────────────────────────────────────────────────╯
`

func TestParsePromptOptions(t *testing.T) {
	p := ParsePrompt(bashPrompt)
	if p.Kind != app.PromptAsk {
		t.Fatalf("kind = %q, want ask", p.Kind)
	}
	if p.Question != "Do you want to proceed?" {
		t.Fatalf("question = %q", p.Question)
	}
	want := []string{"Yes", "Yes, and don't ask again for go test commands in /home/me/src/saddle", "No, and tell Claude what to do differently (esc)"}
	if len(p.Options) != len(want) {
		t.Fatalf("options = %+v, want %d", p.Options, len(want))
	}
	for i, o := range p.Options {
		if o.N != i+1 || o.Label != want[i] {
			t.Errorf("option %d = %+v, want %d %q", i, o, i+1, want[i])
		}
	}
}

// TestParsePromptTakesTheLastList: a numbered list earlier on the screen
// (a plan the agent printed) is not the prompt's options.
func TestParsePromptTakesTheLastList(t *testing.T) {
	screen := "Plan:\n1. read the code\n2. write tests\n3. fix it\n\nWhich approach should I take?\n❯ 1. Rewrite the parser\n  2. Patch the regex\n"
	p := ParsePrompt(screen)
	if p.Question != "Which approach should I take?" || len(p.Options) != 2 || p.Options[1].Label != "Patch the regex" {
		t.Fatalf("prompt = %+v", p)
	}
}

func TestParsePromptTrust(t *testing.T) {
	screen := "Do you trust the files in this folder?\n\n/home/me/x\n\nIs this a project you created or one you trust?\n❯ 1. Yes, proceed\n  2. No, exit\n\nEnter to confirm · Esc to exit\n"
	p := ParsePrompt(screen)
	if p.Kind != app.PromptTrust || len(p.Options) != 2 || p.Options[0].Label != "Yes, proceed" {
		t.Fatalf("prompt = %+v", p)
	}
}

func TestParsePromptNone(t *testing.T) {
	if p := ParsePrompt("● Done. All tests pass.\n> "); p.Kind != app.PromptNone || len(p.Options) != 0 {
		t.Fatalf("prompt = %+v, want none", p)
	}
}

// TestParsePromptScrubsAndClips: options come from an agent's screen, so
// they are scrubbed of secrets and clipped like any other item text.
func TestParsePromptScrubsAndClips(t *testing.T) {
	screen := "Do you want to proceed?\n❯ 1. Yes, run curl -H 'Authorization: Bearer ghp_abcdefghijklmnopqrstuvwxyz0123456789'\n  2. " + strings.Repeat("x", 900) + "\n"
	p := ParsePrompt(screen)
	if len(p.Options) != 2 {
		t.Fatalf("options = %+v", p.Options)
	}
	if strings.Contains(p.Options[0].Label, "ghp_") {
		t.Fatalf("secret leaked into option: %q", p.Options[0].Label)
	}
	if n := len([]rune(p.Options[1].Label)); n > maxOption {
		t.Fatalf("option is %d runes, want at most %d", n, maxOption)
	}
}

// TestNeedsYouItemIDs: each item has an id that is stable while the prompt
// is unchanged and changes when the prompt does, so an answer bound to it
// can't land on a newer prompt. Notices are keyed by their notice id.
func TestNeedsYouItemIDs(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, tk := range []store.Task{
		{ID: app.OrchestratorID, Title: "orchestrator", Role: store.RoleOrchestrator, Status: store.Running},
		{ID: "t1", Title: "meter", Role: store.RoleWorker, Status: store.Running},
	} {
		if err := st.CreateTask(tk); err != nil {
			t.Fatal(err)
		}
	}
	st.Event("t1", "notification", "Claude needs your permission to use Bash")
	if err := st.SetStatus("t1", store.NeedsYou); err != nil {
		t.Fatal(err)
	}
	if err := st.Notify(app.OrchestratorID, store.NoticeAction, "t2 escalated"); err != nil {
		t.Fatal(err)
	}
	screen := bashPrompt
	read := func() []NeedsYouItem {
		t.Helper()
		items, err := NeedsYouFrom(st, func(store.Task) string { return screen })
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 {
			t.Fatalf("items = %+v", items)
		}
		return items
	}
	a := read()
	if a[0].ID == "" || a[1].ID == "" || a[0].ID == a[1].ID {
		t.Fatalf("ids = %q %q, want distinct and set", a[0].ID, a[1].ID)
	}
	if a[0].Question != "Do you want to proceed?" || len(a[0].Options) != 3 {
		t.Fatalf("prompt item = %+v, want the parsed question and options", a[0])
	}
	if !strings.HasPrefix(a[1].ID, "n") {
		t.Fatalf("notice id = %q", a[1].ID)
	}
	if b := read(); b[0].ID != a[0].ID || b[1].ID != a[1].ID {
		t.Fatalf("ids changed between reads of the same state: %q %q", b[0].ID, b[1].ID)
	}
	screen = strings.Replace(bashPrompt, "go test ./internal/remote/...", "rm -rf build", 1)
	// Same question and options, different command: a different prompt.
	if c := read(); c[0].ID == a[0].ID {
		t.Fatalf("a prompt for another command kept id %q", a[0].ID)
	}
}
