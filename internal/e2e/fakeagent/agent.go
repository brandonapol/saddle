package fakeagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Agent runs one Script the way Claude Code would run a session.
type Agent struct {
	Dir      string // the script dir
	Task     string
	Work     string // the worktree
	Saddle   string // the saddle binary; "" means saddle on PATH
	Settings string // Claude Code settings.json with the hooks; "" means no hooks
	Prompt   string
	In       io.Reader
	Out      io.Writer

	mu     sync.Mutex
	logf   *os.File
	hooks  map[string][]string
	inbox  []string
	cursor int
	lines  chan string
}

// Version is what --version prints, shaped like Claude Code's.
const Version = "2.1.0 (Claude Code; fakeagent)"

// idleForever is a wait nothing matches: after the script the agent idles.
const idleForever = "\x00idle"

// Main parses Claude Code's command line and runs the agent; it returns the
// exit code. --script-dir is the only flag of its own.
func Main(args []string, stdin io.Reader, stdout io.Writer) int {
	if slices.Contains(args, "--version") || slices.Contains(args, "-v") {
		fmt.Fprintln(stdout, Version)
		return 0
	}
	a := &Agent{Task: os.Getenv("SADDLE_TASK"), In: stdin, Out: stdout}
	a.Work, _ = os.Getwd()
	headless := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-p" || arg == "--print":
			headless = true
		case arg == "--verbose" || arg == "--include-partial-messages":
		case strings.HasPrefix(arg, "-"):
			if i+1 >= len(args) {
				break
			}
			v := args[i+1]
			i++
			switch arg {
			case "--script-dir":
				a.Dir = v
			case "--settings":
				a.Settings = v
			}
		default:
			a.Prompt = arg
		}
	}
	if a.Dir == "" {
		fmt.Fprintln(os.Stderr, "fakeagent: --script-dir is required")
		return 2
	}
	if headless {
		return a.headless()
	}
	if err := a.Start(); err != nil {
		fmt.Fprintln(stdout, "fakeagent:", err)
		return 1
	}
	s, err := Load(a.Dir, a.Task)
	if err != nil {
		a.Log("error", err.Error())
		return 1
	}
	return a.Run(s)
}

// Start opens the log, reads the hooks and starts reading input.
func (a *Agent) Start() error {
	f, err := os.OpenFile(LogPath(a.Dir, a.Task), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	a.logf = f
	if a.Settings != "" {
		if err := a.readHooks(); err != nil {
			return err
		}
	}
	a.lines = make(chan string)
	go func() {
		sc := bufio.NewScanner(a.In)
		for sc.Scan() {
			a.lines <- sc.Text()
		}
		close(a.lines)
	}()
	return nil
}

// Log appends one line to the agent's log and echoes it to the screen.
func (a *Agent) Log(kind, text string) {
	line := kind + ": " + strings.ReplaceAll(strings.TrimSpace(text), "\n", " | ")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logf != nil {
		fmt.Fprintln(a.logf, line)
	}
	fmt.Fprintf(a.Out, "[fakeagent %s] %s\n", a.Task, line)
}

func (a *Agent) readHooks() error {
	b, err := os.ReadFile(a.Settings)
	if err != nil {
		return err
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("%s: %w", a.Settings, err)
	}
	a.hooks = map[string][]string{}
	for ev, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				a.hooks[ev] = append(a.hooks[ev], h.Command)
			}
		}
	}
	return nil
}

// hookOut is what a Claude Code hook may print.
type hookOut struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	Specific struct {
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
		AdditionalContext        string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// hook runs every command for event with Claude Code's payload. Context the
// hooks add lands in the inbox, as it would in Claude's conversation.
func (a *Agent) hook(event string, extra map[string]any) hookOut {
	var res hookOut
	payload := map[string]any{"session_id": "fake-" + a.Task, "hook_event_name": event, "cwd": a.Work}
	for k, v := range extra {
		payload[k] = v
	}
	in, _ := json.Marshal(payload)
	for _, c := range a.hooks[event] {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, "sh", "-c", c)
		cmd.Dir, cmd.Stdin = a.Work, bytes.NewReader(in)
		out, err := cmd.Output()
		cancel()
		if err != nil {
			a.Log("hook-error", event+": "+err.Error())
			continue
		}
		if len(bytes.TrimSpace(out)) == 0 {
			continue
		}
		var o hookOut
		if err := json.Unmarshal(out, &o); err != nil {
			a.Log("hook-error", event+": bad output: "+string(out))
			continue
		}
		if o.Specific.AdditionalContext != "" {
			a.receive("context", o.Specific.AdditionalContext)
		}
		if o.Decision != "" {
			res.Decision, res.Reason = o.Decision, o.Reason
		}
		if o.Specific.PermissionDecision != "" {
			res.Specific = o.Specific
		}
	}
	return res
}

func (a *Agent) receive(kind, text string) {
	a.Log(kind, text)
	a.inbox = append(a.inbox, text)
}

// Run runs s's steps, then idles. It returns only on an Exit step, a failed
// step or the end of input.
func (a *Agent) Run(s Script) int {
	a.Log("prompt", a.Prompt)
	a.hook("SessionStart", map[string]any{"source": "startup"})
	for i, st := range s.Steps {
		if st.Exit != nil {
			a.Log("exit", fmt.Sprint(*st.Exit))
			return *st.Exit
		}
		err := a.step(st)
		if err == nil {
			a.Log("step", fmt.Sprintf("%d %s ok", i+1, st.Kind()))
			continue
		}
		a.Log("step", fmt.Sprintf("%d %s failed: %v", i+1, st.Kind(), err))
		if errors.Is(err, io.EOF) {
			return 1
		}
		if !st.Optional {
			a.Log("stuck", "script stopped at a failed step")
			break
		}
	}
	a.Log("idle", "script finished")
	if err := a.wait(idleForever); err != nil {
		return 0
	}
	return 0
}

func (a *Agent) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(a.Work, p)
}

func (a *Agent) step(st Step) error {
	switch {
	case st.Write != "", st.Append != "":
		p := a.path(st.Write + st.Append)
		tool := map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": p, "content": st.Content}}
		if d := a.hook("PreToolUse", tool); d.Specific.PermissionDecision == "deny" {
			a.Log("denied", d.Specific.PermissionDecisionReason)
			return errors.New("write denied: " + d.Specific.PermissionDecisionReason)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if st.Append != "" {
			flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(p, flag, 0o644)
		if err != nil {
			return err
		}
		_, err = f.WriteString(st.Content)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		a.hook("PostToolUse", tool)
		return err
	case st.Remove != "":
		return a.bash("rm -f -- " + shellQuote(st.Remove))
	case st.Commit != "":
		return a.bash("git add -A && git commit -q -m " + shellQuote(st.Commit))
	case st.Run != "":
		return a.bash(st.Run)
	case st.Done != "":
		return a.bash(shellQuote(a.saddle()) + " done -s " + shellQuote(st.Done))
	case st.Resolve != "":
		if err := os.WriteFile(a.path(st.Resolve), []byte(st.Content), 0o644); err != nil {
			return err
		}
		return a.bash("git add -- " + shellQuote(st.Resolve) + " && GIT_EDITOR=true git rebase --continue")
	case st.MCP != "":
		return a.mcp(st.MCP, st.Args)
	case st.Notify != "":
		a.hook("Notification", map[string]any{"message": st.Notify})
		return nil
	case st.Wait != "":
		return a.wait(st.Wait)
	}
	return nil
}

func (a *Agent) saddle() string {
	if a.Saddle != "" {
		return a.Saddle
	}
	return "saddle"
}

// bash runs cmd in the worktree as Claude's Bash tool would, then the
// PostToolUse hook.
func (a *Agent) bash(cmd string) error {
	c := exec.Command("sh", "-c", cmd)
	c.Dir = a.Work
	out, err := c.CombinedOutput()
	if s := strings.TrimSpace(string(out)); s != "" {
		a.Log("output", s)
	}
	a.hook("PostToolUse", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": cmd}})
	if err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	return nil
}

// mcp calls one tool on `saddle mcp`, as Claude would through --mcp-config.
func (a *Agent) mcp(tool string, args map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.Command(a.saddle(), "mcp")
	cmd.Dir = a.Work
	c := mcp.NewClient(&mcp.Implementation{Name: "fakeagent", Version: "e2e"}, nil)
	s, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	if args == nil {
		args = map[string]any{}
	}
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return err
	}
	var text []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text = append(text, t.Text)
		}
	}
	a.Log("mcp-result", tool+": "+strings.Join(text, " "))
	a.hook("PostToolUse", map[string]any{"tool_name": "mcp__saddle__" + tool, "tool_input": args})
	if res.IsError {
		return errors.New(strings.Join(text, " "))
	}
	return nil
}

// wait ends the turn and idles until something received contains match.
func (a *Agent) wait(match string) error {
	for {
		if i := slices.IndexFunc(a.inbox[a.cursor:], func(s string) bool { return strings.Contains(s, match) }); i >= 0 {
			a.cursor += i + 1
			return nil
		}
		// Each pass ends a turn: Stop may hand back more work.
		if d := a.hook("Stop", map[string]any{"stop_hook_active": false}); d.Decision == "block" {
			a.receive("stop-block", d.Reason)
			continue
		}
		fmt.Fprint(a.Out, "\n> ")
		line, ok := <-a.lines
		if !ok {
			return io.EOF
		}
		a.receive("input", line)
		a.hook("UserPromptSubmit", map[string]any{"prompt": line})
	}
}

// headless speaks Claude Code's stream-json on stdin and stdout, standing in
// for the TUI's orchestrator: it acknowledges every message.
func (a *Agent) headless() int {
	f, err := os.OpenFile(OrchestratorLog(a.Dir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 1
	}
	defer f.Close()
	enc := json.NewEncoder(a.Out)
	_ = enc.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": "fake-orchestrator"})
	sc := bufio.NewScanner(a.In)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		text := fmt.Sprint(m.Message.Content)
		fmt.Fprintln(f, "user: "+strings.ReplaceAll(text, "\n", " | "))
		first, _, _ := strings.Cut(text, "\n")
		if len(first) > 60 {
			first = first[:60]
		}
		_ = enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "fake orchestrator ack: " + first}}}})
		_ = enc.Encode(map[string]any{"type": "result", "subtype": "success", "result": "ok", "session_id": "fake-orchestrator"})
	}
	return 0
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
