package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/store"
)

// Claude Code skills and slash commands from the chat input. "/<skill> args"
// goes to the orchestrator as is; pressing / on an agent in the task list
// aims the next command at that agent's pane instead.

// slashCmd is a parsed "/name args" line.
type slashCmd struct {
	name, args string
}

var slashName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)

// parseSlash reports whether text is a slash command. A path like /tmp/x is
// not one.
func parseSlash(text string) (slashCmd, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return slashCmd{}, false
	}
	name, args := text[1:], ""
	if i := strings.IndexFunc(name, unicode.IsSpace); i >= 0 {
		name, args = name[:i], name[i+1:]
	}
	if !slashName.MatchString(name) {
		return slashCmd{}, false
	}
	return slashCmd{name: name, args: strings.TrimSpace(args)}, true
}

func (c slashCmd) String() string {
	if c.args == "" {
		return "/" + c.name
	}
	return "/" + c.name + " " + c.args
}

// builtinSkills are Claude Code's bundled commands worth completing.
var builtinSkills = []string{"code-review", "compact", "init", "review", "security-review", "simplify"}

// findSkills lists skill and command names visible to Claude Code in root:
// built-ins, user and project skills and commands, and installed plugins'
// (as plugin:name).
func findSkills(home, root string) []string {
	seen := map[string]bool{}
	for _, s := range builtinSkills {
		seen[s] = true
	}
	addDir := func(dir, prefix string) {
		skills, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
		for _, p := range skills {
			seen[prefix+filepath.Base(filepath.Dir(p))] = true
		}
		cmds := filepath.Join(dir, "commands")
		_ = filepath.WalkDir(cmds, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
				return nil
			}
			rel, _ := filepath.Rel(cmds, strings.TrimSuffix(p, ".md"))
			seen[prefix+strings.ReplaceAll(filepath.ToSlash(rel), "/", ":")] = true
			return nil
		})
	}
	if home != "" {
		addDir(filepath.Join(home, ".claude"), "")
		for name, path := range installedPlugins(filepath.Join(home, ".claude", "plugins", "installed_plugins.json")) {
			addDir(path, name+":")
		}
	}
	if root != "" {
		addDir(filepath.Join(root, ".claude"), "")
	}
	names := make([]string, 0, len(seen))
	for s := range seen {
		names = append(names, s)
	}
	sort.Strings(names)
	return names
}

// installedPlugins maps plugin name to install path from Claude Code's
// installed_plugins.json.
func installedPlugins(file string) map[string]string {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var f struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	out := map[string]string{}
	for key, installs := range f.Plugins {
		name, _, _ := strings.Cut(key, "@")
		if len(installs) > 0 && installs[0].InstallPath != "" {
			out[name] = installs[0].InstallPath
		}
	}
	return out
}

// completeSlash completes a "/prefix" input against names. It returns the new
// input and the candidates; the input is unchanged when nothing matches.
func completeSlash(input string, names []string) (string, []string) {
	if !strings.HasPrefix(input, "/") || strings.ContainsAny(input, " \t\n") {
		return input, nil
	}
	prefix := input[1:]
	var matches []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			matches = append(matches, n)
		}
	}
	// Plugin skills also complete on the part after "plugin:".
	if len(matches) == 0 {
		for _, n := range names {
			if _, s, ok := strings.Cut(n, ":"); ok && strings.HasPrefix(s, prefix) {
				matches = append(matches, n)
			}
		}
	}
	switch len(matches) {
	case 0:
		return input, nil
	case 1:
		return "/" + matches[0] + " ", matches
	}
	common := matches[0]
	for _, m := range matches[1:] {
		for !strings.HasPrefix(m, common) {
			common = common[:len(common)-1]
		}
	}
	if len(common) < len(prefix) {
		return input, matches
	}
	return "/" + common, matches
}

// Model glue.

// aimAtAgent points the chat input at the selected agent's pane for one
// slash command.
func (m *model) aimAtAgent() tea.Cmd {
	t, ok := m.selected()
	if !ok || t.Window == "" {
		return func() tea.Msg { return flashMsg("no live window for that task") }
	}
	m.target = t.ID
	m.focus = focusChat
	m.input.Prompt = t.ID + " › "
	m.input.Placeholder = "/skill for " + t.ID + " (esc to cancel)"
	m.input.SetValue("/")
	m.input.Focus()
	return nil
}

// unaim sends the chat input back to the orchestrator.
func (m *model) unaim() {
	m.target = ""
	m.input.Prompt = "› "
	m.input.Placeholder = "Message the orchestrator…"
}

// submitTargeted handles enter while the input is aimed at an agent. Only
// slash commands go to agents; anything else stays in the input.
func (m *model) submitTargeted(text string) tea.Cmd {
	id, a := m.target, m.app
	c, ok := parseSlash(text)
	if !ok {
		msg := flashMsg("only /commands go to " + id + "; esc sends to the orchestrator")
		return func() tea.Msg { return msg }
	}
	m.input.Reset()
	m.unaim()
	m.addChat(store.ChatEvent, c.String()+" → "+id)
	return func() tea.Msg {
		if err := a.SendKeys(id, c.String(), nil); err != nil {
			return flashMsg(c.String() + ": " + err.Error())
		}
		return flashMsg("sent " + c.String() + " to " + id)
	}
}

// completeInput completes a skill name in the chat input. It reports false
// when the input isn't a slash command, so tab keeps its usual meaning.
func (m *model) completeInput() (tea.Cmd, bool) {
	v := m.input.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t\n") {
		return nil, false
	}
	if m.skills == nil {
		home, _ := os.UserHomeDir()
		m.skills = findSkills(home, m.app.Root)
	}
	nv, matches := completeSlash(v, m.skills)
	m.input.SetValue(nv)
	switch {
	case len(matches) == 0:
		return func() tea.Msg { return flashMsg("no skill matches " + v) }, true
	case len(matches) > 1:
		if len(matches) > 8 {
			matches = append(matches[:8], "…")
		}
		return func() tea.Msg { return flashMsg(strings.Join(matches, "  ")) }, true
	}
	return nil, true
}
