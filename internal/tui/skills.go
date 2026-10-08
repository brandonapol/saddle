package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/orch"
	"github.com/brandonapol/saddle/internal/store"
)

// Claude Code skills and slash commands from the chat input. Typing / opens
// a menu of the orchestrator's commands, as its session reports them.
// "/<skill> args" goes to the orchestrator as is, and Claude Code expands it
// (stream-json input does, #255); the TUI's own commands (/help) stay here.
// Pressing / on an agent in the task list aims the next command at that
// agent's pane instead: workers run interactive Claude Code, which has the
// same skills and every tool.

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

// builtinSkills are Claude Code's bundled commands, for the list shown
// before the session reports its own.
var builtinSkills = []string{"batch", "code-review", "compact", "debug", "init", "loop", "security-review", "simplify", "verify"}

// tuiCommands are slash commands the TUI handles itself; they never reach
// the orchestrator.
var tuiCommands = []slashEntry{{name: "help", desc: "show saddle's keys (saddle)"}}

// skillNeeds lists the tools bundled skills call that a deny list may
// remove, so the chat can say why one can't finish (#255).
var skillNeeds = map[string][]string{
	"loop":     {"CronCreate", "ScheduleWakeup", "Monitor"},
	"simplify": {"Agent", "Edit"},
	"batch":    {"Agent", "Write"},
	"init":     {"Write"},
}

// slashEntry is one row of the / autocomplete.
type slashEntry struct {
	name, desc, hint string
}

// hintDesc is the text after the name in the menu.
func (e slashEntry) hintDesc() string {
	s := ""
	if e.hint != "" {
		s = " " + e.hint
	}
	if e.desc != "" {
		s += "  " + e.desc
	}
	return s
}

// findSkills lists skill and command names visible to Claude Code in root:
// built-ins, user and project skills and commands, and installed plugins'
// (as plugin:name). It stands in until the session lists its own.
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
		cfg := claudeConfigDir(home)
		addDir(cfg, "")
		for name, path := range installedPlugins(filepath.Join(cfg, "plugins", "installed_plugins.json")) {
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

// claudeConfigDir is where Claude Code keeps user config: CLAUDE_CONFIG_DIR,
// else ~/.claude.
func claudeConfigDir(home string) string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(home, ".claude")
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

// slashMatches lists the entries a "/prefix" input completes to: those
// whose name starts with prefix, else plugin skills whose part after
// "plugin:" does.
func slashMatches(prefix string, es []slashEntry) []slashEntry {
	var out []slashEntry
	for _, e := range es {
		if strings.HasPrefix(e.name, prefix) {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		for _, e := range es {
			if _, s, ok := strings.Cut(e.name, ":"); ok && strings.HasPrefix(s, prefix) {
				out = append(out, e)
			}
		}
	}
	return out
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
	m.target, m.askScreen = "", false
	m.input.Prompt = "› "
	m.input.Placeholder = "Message the orchestrator…"
}

// submitTargeted handles enter while the input is aimed at an agent. Only
// slash commands go to agents; anything else stays in the input.
func (m *model) submitTargeted(text string) tea.Cmd {
	if m.asking() {
		return m.submitAsk(text)
	}
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

// setCommands replaces the completion list with the session's commands,
// keeping descriptions an earlier report had when this one has none (init
// lists names only).
func (m *model) setCommands(cs []orch.Command) {
	if len(cs) == 0 {
		return
	}
	old := map[string]slashEntry{}
	for _, e := range m.commands {
		old[e.name] = e
	}
	m.commands = make([]slashEntry, 0, len(cs))
	for _, c := range cs {
		e := slashEntry{name: c.Name, desc: c.Description, hint: c.ArgHint}
		if e.desc == "" {
			e.desc, e.hint = old[c.Name].desc, old[c.Name].hint
		}
		m.commands = append(m.commands, e)
	}
}

// slashEntries is everything / completes to: the session's commands (or,
// until it reports them, the skills on disk) and the TUI's own, by name.
func (m *model) slashEntries() []slashEntry {
	es := append([]slashEntry(nil), m.commands...)
	if len(es) == 0 {
		if m.diskSkills == nil {
			home, root := "", ""
			home, _ = os.UserHomeDir()
			if m.app != nil {
				root = m.app.Root
			}
			m.diskSkills = findSkills(home, root)
		}
		for _, n := range m.diskSkills {
			es = append(es, slashEntry{name: n})
		}
	}
	seen := map[string]bool{}
	for _, e := range es {
		seen[e.name] = true
	}
	for _, e := range tuiCommands {
		if !seen[e.name] {
			es = append(es, e)
		}
	}
	sort.SliceStable(es, func(i, j int) bool { return es[i].name < es[j].name })
	return es
}

// slashMenu is the open autocomplete: the entries the chat input completes
// to while it holds a bare "/prefix". It is nil when closed.
func (m *model) slashMenu() []slashEntry {
	v := m.input.Value()
	if m.focus != focusChat || m.asking() || !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t\n") {
		return nil
	}
	ms := slashMatches(v[1:], m.slashEntries())
	m.slashSel = min(max(m.slashSel, 0), max(len(ms)-1, 0))
	return ms
}

// slashMenuRows is how many rows of the menu are drawn.
const slashMenuRows = 6

// viewSlashMenu renders the open menu at width w, scrolled to keep the
// pick in view; "" when it is closed.
func (m *model) viewSlashMenu(w int) string {
	ms := m.slashMenu()
	if len(ms) == 0 {
		return ""
	}
	top := max(0, m.slashSel-slashMenuRows+1)
	var rows []string
	for i := top; i < len(ms) && i < top+slashMenuRows; i++ {
		e := ms[i]
		name, rest := "/"+e.name, e.hintDesc()
		name = truncate(name, w-2)
		if room := w - 2 - len([]rune(name)); room < len([]rune(rest)) {
			rest = ""
			if room > 4 {
				rest = truncate(e.hintDesc(), room)
			}
		}
		mark, st := "  ", sKey
		if i == m.slashSel {
			mark, st = sKey.Render("› "), lipgloss.NewStyle().Foreground(cFocus).Bold(true)
		}
		rows = append(rows, mark+st.Render(name)+sDim.Render(rest))
	}
	if len(ms) > slashMenuRows {
		rows = append(rows, sDim.Render(fmt.Sprintf("  %d of %d · ↑↓ pick · tab completes", m.slashSel+1, len(ms))))
	}
	return strings.Join(rows, "\n")
}

// slashKey moves the menu's pick with the arrows. It reports false when
// the menu is closed or the key isn't an arrow.
func (m *model) slashKey(k tea.KeyMsg) bool {
	if k.Type != tea.KeyUp && k.Type != tea.KeyDown {
		return false
	}
	ms := m.slashMenu()
	if len(ms) == 0 {
		return false
	}
	if k.Type == tea.KeyUp {
		m.slashSel = (m.slashSel - 1 + len(ms)) % len(ms)
	} else {
		m.slashSel = (m.slashSel + 1) % len(ms)
	}
	return true
}

// completeInput completes the picked command in the chat input. It reports
// false when the input isn't a bare slash command, so tab keeps its usual
// meaning.
func (m *model) completeInput() (tea.Cmd, bool) {
	v := m.input.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t\n") {
		return nil, false
	}
	ms := m.slashMenu()
	if len(ms) == 0 {
		return func() tea.Msg { return flashMsg("no skill or command matches " + v) }, true
	}
	m.input.SetValue("/" + ms[m.slashSel].name + " ")
	m.input.CursorEnd()
	m.slashSel = 0
	return nil, true
}

// sendSlash sends a slash command typed for the orchestrator. The TUI's own
// commands run here. Claude Code expands the rest itself, in stream-json
// too, so they go as typed; a bundled skill that needs tools the
// orchestrator lacks says so first.
func (m *model) sendSlash(c slashCmd) {
	switch c.name {
	case "help":
		m.helpOpen = true
		return
	}
	m.sendUser(c.String())
	var missing []string
	for _, tool := range skillNeeds[c.name] {
		if slices.Contains(m.launch.Deny, tool) {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		m.addChat(store.ChatEvent, fmt.Sprintf("/%s uses %s, which the orchestrator can't; it will do what it can without them. "+
			"To run it fully, press / on an agent row and send it to a worker.", c.name, strings.Join(missing, ", ")))
	}
}
