package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
)

// Kinds of prompt an agent's screen can be stuck on.
const (
	PromptNone  = ""
	PromptTrust = "trust" // Claude Code's "do you trust this folder" dialog
	PromptAsk   = "ask"   // a permission prompt or a question with options
)

var (
	trustRe  = regexp.MustCompile(`(?i)trust this folder|Is this a project you created or one you trust`)
	promptRe = regexp.MustCompile(`(?m)Enter to confirm|Esc to cancel|Do you want to (proceed|make this edit|create|run)|^\s*❯\s*\d+\.\s|` + codexPrompt + `|` + grokPrompt + `|` + openCodePrompt)
)

// codexPrompt is Codex's approval and trust screens (#480): a "› 1." menu
// and their wordings. The trust screen counts as an ask, not PromptTrust:
// Down, Enter there picks "No, ask me to approve".
const codexPrompt = `^\s*›\s*\d+\.\s|Press enter to (confirm|continue)|Would you like to (run|make|apply)|trust the contents of this directory|allow Codex to work in this folder`

// grokPrompt is the Grok CLI's approval screen (#488): radio options drawn
// as "┃  1 (●) Yes, proceed" inside a bar, with no wording the others share.
const grokPrompt = `^[\s┃]*\d+ \([●○]\)\s`

// openCodePrompt is opencode's permission panel (#520): "△ Permission
// required" and the Allow once / Allow always / Reject row. There is no
// numbered menu.
const openCodePrompt = `Permission required|Allow once\s+Allow always\s+Reject`

// DetectPrompt reports whether a terminal screen is showing an interactive prompt.
func DetectPrompt(screen string) string {
	switch {
	case trustRe.MatchString(screen):
		return PromptTrust
	case promptRe.MatchString(screen):
		return PromptAsk
	}
	return PromptNone
}

// RootTrusted reports whether the user has already trusted the main checkout
// in Claude Code. Worktrees are copies of it, so their trust prompt can be
// accepted on the user's behalf. ~/.claude.json is only read, never written.
func (a *App) RootTrusted() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		Projects map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return false
	}
	// Trust granted on the repo or any directory above it counts.
	for dir := filepath.Clean(a.Root); ; dir = filepath.Dir(dir) {
		if p, ok := cfg.Projects[dir]; ok && p.HasTrustDialogAccepted {
			return true
		}
		if dir == filepath.Dir(dir) {
			return false
		}
	}
}
