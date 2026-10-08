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

func TestModelScope(t *testing.T) {
	for body, want := range map[string]string{
		"":                                    "opus",
		"**Model scope: Opus.** big":          "opus",
		"**Model scope: Sonnet.** small edit": "sonnet",
		"model scope: sonnet":                 "sonnet",
		"Mentions Sonnet but no scope line":   "opus",
	} {
		if got := ModelScope(body); got != want {
			t.Errorf("ModelScope(%q) = %q, want %q", body, got, want)
		}
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
	if p := Prompt(Issue{Number: 1, Title: "t"}, "", ""); !strings.Contains(p, "**Model scope: Opus.**") {
		t.Errorf("a ticket without a scope line gets none in the prompt:\n%s", p)
	}
}
