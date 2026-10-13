package usage

import (
	"slices"
	"strings"
	"unicode"
)

// CodexModels are the Codex model lines the usage panel colours apart (#482).
var CodexModels = []string{"luna", "terra", "sol", "astra"}

// ModelIs reports whether model is name: the same id, ignoring case, or an
// id with name as one of its words ("gpt-6-astra" is astra, "console" is
// not sol).
func ModelIs(model, name string) bool {
	model, name = strings.ToLower(strings.TrimSpace(model)), strings.ToLower(strings.TrimSpace(name))
	if model == "" || name == "" {
		return false
	}
	if model == name {
		return true
	}
	return slices.Contains(strings.FieldsFunc(model, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}), name)
}

// CodexModelName labels a Codex model for the usage panel: one of
// CodexModels when the id names it, else the id itself. An empty or
// placeholder model is "" (the rollout didn't say).
func CodexModelName(model string) string {
	for _, n := range CodexModels {
		if ModelIs(model, n) {
			return n
		}
	}
	if model == CodexModel {
		return ""
	}
	return strings.TrimSpace(model)
}
