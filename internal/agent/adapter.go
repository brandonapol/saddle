package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brandonapol/saddle/internal/usage"
)

// Adapter starts one kind of coding agent and talks to it.
type Adapter interface {
	// Name is the adapter's config and spawn name, e.g. "claude".
	Name() string
	// Launch writes l's launch files and returns the shell command that runs
	// the agent interactively in a tmux window.
	Launch(l Launch) (string, error)
	// Inject returns the text to type into the agent's idle window to deliver
	// notices, already formatted as one block.
	Inject(notices string) string
	// Usage says where the agent's transcripts live and how to parse them.
	Usage() UsageSource
	// Hooks reports whether the agent runs `saddle hook`: writes are checked
	// against claims and notices arrive with tool calls. Without hooks claims
	// are advisory and notices are typed in whole.
	Hooks() bool
}

// UsageSource locates an agent's transcript for usage metering.
type UsageSource struct {
	Agent string // a usage agent kind: usage.Claude, usage.Codex or usage.Grok
	// Transcript returns the transcript of session for an agent working in
	// dir and launched from runDir. session is empty for agents whose hooks
	// don't report one. It returns a best guess, or "" when there is nothing
	// to read yet.
	Transcript func(dir, runDir, session string) string
}

// WakeLine is typed into an idle agent whose hook will deliver its notices.
const WakeLine = "[saddle] You have new notices. Read them and act on them."

// ByName returns the adapter for name; empty means Claude Code.
func ByName(name string) (Adapter, error) {
	if name == "" {
		name = usage.Claude
	}
	if a, ok := adapters()[name]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("unknown adapter %q: want one of %s", name, strings.Join(Names(), ", "))
}

// Names lists the known adapters.
func Names() []string {
	var out []string
	for n := range adapters() {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func adapters() map[string]Adapter {
	return map[string]Adapter{usage.Claude: Claude{}, usage.Codex: Codex{}, usage.Grok: Grok{}}
}

// Claude is the Claude Code adapter: hooks and the saddle MCP server are wired
// through --settings and --mcp-config, the brief is appended to the system
// prompt and the task prompt is the first message.
type Claude struct {
	// ConfigDir is Claude Code's config dir. Empty means $CLAUDE_CONFIG_DIR,
	// then ~/.claude.
	ConfigDir string
}

func (Claude) Name() string         { return usage.Claude }
func (Claude) Inject(string) string { return WakeLine }
func (Claude) Hooks() bool          { return true }
func (Claude) Launch(l Launch) (string, error) {
	if err := l.record(usage.Claude); err != nil {
		return "", err
	}
	return l.Write()
}

func (c Claude) Usage() UsageSource {
	return UsageSource{Agent: usage.Claude, Transcript: c.transcript}
}

func (c Claude) transcript(dir, _, session string) string {
	cfg := c.ConfigDir
	if cfg == "" {
		cfg = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	p := usage.ClaudeTranscriptPath(cfg, dir, session)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if f := usage.FindClaudeTranscript(cfg, session); f != "" {
		return f
	}
	return p
}

// adapterFile records which adapter a task's run dir was launched with.
const adapterFile = "adapter"

func (l Launch) record(name string) error {
	if err := os.MkdirAll(l.RunDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(l.RunDir, adapterFile), []byte(name+"\n"), 0o644)
}

// Recorded returns the adapter name a run dir was launched with; Claude Code
// for run dirs written before adapters were recorded.
func Recorded(runDir string) string {
	b, err := os.ReadFile(filepath.Join(runDir, adapterFile))
	if err != nil || strings.TrimSpace(string(b)) == "" {
		return usage.Claude
	}
	return strings.TrimSpace(string(b))
}
