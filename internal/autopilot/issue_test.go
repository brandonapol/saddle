package autopilot

import (
	"slices"
	"strings"
	"testing"
)

func TestParseClaims(t *testing.T) {
	cases := []struct {
		body string
		want []string
	}{
		{"no claims here", nil},
		{"Claims: internal/foo/**, `cmd/x.go`\n", []string{"internal/foo/**", "cmd/x.go"}},
		{"**Claims:** internal/a/** internal/b.go", []string{"internal/a/**", "internal/b.go"}},
		{"intro\nclaims:\n- `internal/bar/**`\n- docs/BAR.md\n\nmore text", []string{"internal/bar/**", "docs/BAR.md"}},
		// The section a GitHub issue form renders.
		{"### Goal\n\nx\n\n### Claims\n\ninternal/a/**\n- `docs/a.md`\n\n### Acceptance\n\ny", []string{"internal/a/**", "docs/a.md"}},
		{"### Claims\r\n\r\n_No response_\r\n\r\n### Acceptance\r\n\r\ninternal/x.go", nil},
	}
	for _, c := range cases {
		if got := ParseClaims(c.body); !slices.Equal(got, c.want) {
			t.Errorf("ParseClaims(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestParseAfter(t *testing.T) {
	if got := ParseAfter("x\nAfter: #12, #7\nafter #3"); !slices.Equal(got, []int{12, 7, 3}) {
		t.Fatalf("ParseAfter = %v", got)
	}
	if got := ParseAfter("comes after lunch #4"); got != nil {
		t.Fatalf("prose matched: %v", got)
	}
}

// #505: Model scope is read as a hint; Difficulty, Needs and Provider are
// what route a ticket.
func TestModelScope(t *testing.T) {
	for body, want := range map[string]string{
		"":                                    "",
		"**Model scope: Opus.** big":          "opus",
		"**Model scope: Sonnet.** small edit": "sonnet",
		"model scope: sonnet":                 "sonnet",
		"Mentions Sonnet but no scope line":   "",
	} {
		if got := ModelScope(body); got != want {
			t.Errorf("ModelScope(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestTicketDifficultyNeedsProvider(t *testing.T) {
	for body, want := range map[string]string{
		"": "",
		"**Difficulty: hard (design judgment).**":             "hard",
		"Difficulty: hard-design":                             "hard",
		"**Difficulty: mechanical.** rename":                  "mechanical",
		"### Difficulty\n\nstandard\n\n### Claims":            "standard",
		"**Model scope: Opus.** old ticket":                   "",
		"**Model scope: Sonnet.** old ticket":                 "standard",
		"Model scope: Haiku":                                  "mechanical",
		"**Model scope: Opus.**\n**Difficulty: mechanical.**": "mechanical",
	} {
		if got := Difficulty(body); got != want {
			t.Errorf("Difficulty(%q) = %q, want %q", body, got, want)
		}
	}
	if got := Needs("x\n**Needs:** Review, image-gen, hooks-required\n"); !slices.Equal(got, []string{"review", "image-gen", "hooks"}) {
		t.Errorf("Needs = %q", got)
	}
	if got := Needs("no needs here"); got != nil {
		t.Errorf("Needs without a line = %q", got)
	}
	if got := Provider("- Provider: codex (needs its plugin)"); got != "codex" {
		t.Errorf("Provider = %q", got)
	}
	if got := Provider("Provider: nobody"); got != "" {
		t.Errorf("unknown provider = %q", got)
	}
}

func TestPrompt(t *testing.T) {
	is := Issue{Number: 9, Title: "Fix it", Body: "**Model scope: Sonnet.** do the thing"}
	p := Prompt(is, "Rule one.", "make check passes")
	for _, want := range []string{"#9", "Fix it", "do the thing", "Rule one.", "make check passes", "Model scope: Sonnet"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q:\n%s", want, p)
		}
	}
	if p := Prompt(Issue{Number: 1, Title: "t"}, "", ""); strings.Contains(p, "Model scope") {
		t.Errorf("a ticket without a scope line gets one in the prompt:\n%s", p)
	}
}
