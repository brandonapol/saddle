package config

import (
	"slices"
	"strings"
)

// Capability-based routing (#505): a ticket states its difficulty and what
// it needs, and the chooser picks the adapter. Each adapter declares the
// difficulties it suits, its capabilities and a model per difficulty in
// [adapters.<name>]; unset keys take the built-in defaults below.

// Ticket difficulties, from a ticket's "Difficulty:" line or spawn's
// difficulty=.
const (
	DifficultyHard       = "hard"       // design judgment, cross-package refactors, careful review
	DifficultyStandard   = "standard"   // a well-scoped feature or fix
	DifficultyMechanical = "mechanical" // a well-specified change with no design judgment
)

// Difficulties lists the difficulties in order, hardest first.
func Difficulties() []string {
	return []string{DifficultyHard, DifficultyStandard, DifficultyMechanical}
}

// NormalizeDifficulty maps how people write a difficulty to one of
// Difficulties: "hard-design", "Hard (design judgment)" and "hard" are
// hard; "easy" and "trivial" are mechanical. Unknown words are "".
func NormalizeDifficulty(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexFunc(s, func(r rune) bool { return r == ' ' || r == '(' || r == '.' || r == ',' }); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(s, "*_`")
	switch {
	case s == "hard" || strings.HasPrefix(s, "hard-") || s == "design":
		return DifficultyHard
	case s == "standard" || s == "medium" || s == "normal":
		return DifficultyStandard
	case s == "mechanical" || s == "easy" || s == "trivial" || s == "simple":
		return DifficultyMechanical
	}
	return ""
}

// Capabilities an adapter may declare and a ticket's "Needs:" may ask for.
// The list is open: any word works when an adapter declares it and a ticket
// needs it. These are the ones saddle knows by default. Non-code hosts
// (image generation from another account, ChatGPT plugins) plug in here: an
// adapter whose CLI drives such a host declares the capability, e.g.
// capabilities = ["image-gen", "plugin-host"], and tickets that need it
// route there.
const (
	CapReview      = "review"       // careful code review
	CapLongContext = "long-context" // a very large context window
	CapImageGen    = "image-gen"    // generates images
	CapWebResearch = "web-research" // searches and reads the web
	CapPluginHost  = "plugin-host"  // runs third-party plugins (e.g. ChatGPT plugins)
	// CapHooks is met by adapters that run saddle's hooks, which enforce
	// claims on every write. It is not declared; the adapter has it or not.
	CapHooks = "hooks"
)

// NormalizeNeed maps a "Needs:" word to a capability: lower case, and
// "hooks-required" is CapHooks.
func NormalizeNeed(s string) string {
	s = strings.ToLower(strings.Trim(strings.TrimSpace(s), "*_`.,"))
	switch s {
	case "hooks-required", "hooks", "hook":
		return CapHooks
	case "image", "images", "image-generation":
		return CapImageGen
	case "web", "research", "web-search":
		return CapWebResearch
	case "plugins", "plugin":
		return CapPluginHost
	}
	return s
}

// Routing is one adapter's built-in capability table.
type Routing struct {
	Difficulties []string
	Capabilities []string
	Models       map[string]string // difficulty -> model
}

// DefaultRouting is each adapter's table when [adapters.<name>] leaves a
// key unset. Claude takes every difficulty and runs hard work on opus and
// mechanical work on sonnet (standard keeps [claude] model); the others
// take standard and mechanical work, so hard work prefers claude while it
// has quota and the rest spreads by remaining quota.
var DefaultRouting = map[string]Routing{
	HarnessClaude: {
		Difficulties: Difficulties(),
		Capabilities: []string{CapReview, CapLongContext},
		Models:       map[string]string{DifficultyHard: "opus", DifficultyMechanical: "sonnet"},
	},
	HarnessCodex:    {Difficulties: []string{DifficultyStandard, DifficultyMechanical}, Capabilities: []string{CapReview}},
	HarnessGrok:     {Difficulties: []string{DifficultyStandard, DifficultyMechanical}, Capabilities: []string{CapWebResearch}},
	"gemini":        {Difficulties: []string{DifficultyStandard, DifficultyMechanical}, Capabilities: []string{CapLongContext, CapWebResearch}},
	AdapterOpenCode: {Difficulties: []string{DifficultyStandard, DifficultyMechanical}},
}

// AdapterDifficulties is the difficulties adapter suits.
func (c Config) AdapterDifficulties(adapter string) []string {
	if ad, ok := c.Adapters[adapter]; ok && ad.Difficulties != nil {
		return ad.Difficulties
	}
	if r, ok := DefaultRouting[adapter]; ok {
		return r.Difficulties
	}
	return Difficulties()
}

// SuitsDifficulty reports whether adapter suits difficulty d; any adapter
// suits a task with no difficulty.
func (c Config) SuitsDifficulty(adapter, d string) bool {
	return d == "" || slices.Contains(c.AdapterDifficulties(adapter), d)
}

// AdapterCapabilities is what adapter declares it can do beyond code.
func (c Config) AdapterCapabilities(adapter string) []string {
	if ad, ok := c.Adapters[adapter]; ok && ad.Capabilities != nil {
		return ad.Capabilities
	}
	return DefaultRouting[adapter].Capabilities
}

// DifficultyModel is the model adapter runs a task of difficulty d on, or
// "" to leave it to the adapter's default. A configured entry replaces the
// built-in one; an excluded model (#482) is never returned.
func (c Config) DifficultyModel(adapter, d string) string {
	if d == "" {
		return ""
	}
	m, ok := c.Adapters[adapter].Models[d]
	if !ok {
		m = DefaultRouting[adapter].Models[d]
	}
	if m == "" || c.ExcludedModel(adapter, m) != "" {
		return ""
	}
	return m
}
