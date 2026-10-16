// Package fakecodex is a stand-in for the Codex CLI's headless mode, for
// journeys that run saddle's orchestrator on codex (#668). It answers
// `codex exec --json [options] [resume <id>] -` the way codex does: JSONL
// events on stdout, a rollout under $CODEX_HOME/sessions, and the saddle
// MCP server started from the -c mcp_servers.saddle.* overrides, as codex
// starts the servers in its config.
//
// A prompt line `CALL <tool> <json args>` calls that saddle tool over MCP. A
// line `TOKENS <n>` makes the turn's rollout report an n-token prompt, so
// the context gauge and compact nudge can be driven. Every prompt is
// appended to $CODEX_HOME/fakecodex.log.
package fakecodex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Window is the model_context_window the fake's rollouts report.
const Window = 200000

// Model is the model the fake's rollouts name when exec gets no -m.
const Model = "gpt-5-codex"

// Main runs the fake on args and returns its exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if slices.Contains(args, "--version") {
		fmt.Fprintln(stdout, "codex-cli 0.0.0 (fakecodex)")
		return 0
	}
	if len(args) == 0 || args[0] != "exec" {
		fmt.Fprintln(stderr, "fakecodex: only exec is faked")
		return 2
	}
	if slices.Contains(args, "--help") {
		fmt.Fprintln(stdout, "Usage: codex exec [OPTIONS] [PROMPT]\n      --json  Print events to stdout as JSONL")
		return 0
	}
	e, err := parse(args[1:])
	if err != nil {
		fmt.Fprintln(stderr, "fakecodex:", err)
		return 2
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "fakecodex:", err)
		return 1
	}
	if err := e.turn(string(b), stdout); err != nil {
		fmt.Fprintln(stderr, "fakecodex:", err)
		return 1
	}
	return 0
}

type execArgs struct {
	resume string
	model  string
	cmd    string
	args   []string
	env    []string
}

// parse reads the exec options saddle's bridge passes.
func parse(args []string) (execArgs, error) {
	var e execArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "resume":
			e.resume = next()
		case "-m", "--model":
			e.model = next()
		case "-c", "--config":
			if err := e.config(next()); err != nil {
				return e, err
			}
		}
	}
	return e, nil
}

func (e *execArgs) config(kv string) error {
	k, v, ok := strings.Cut(kv, "=")
	if !ok {
		return nil
	}
	switch k {
	case "mcp_servers.saddle.command":
		s, err := strconv.Unquote(v)
		if err != nil {
			return fmt.Errorf("%s: %w", kv, err)
		}
		e.cmd = s
	case "mcp_servers.saddle.args":
		return json.Unmarshal([]byte(v), &e.args)
	case "mcp_servers.saddle.env":
		for _, p := range strings.Split(strings.Trim(v, "{}"), ",") {
			name, q, _ := strings.Cut(p, "=")
			val, err := strconv.Unquote(strings.TrimSpace(q))
			if err != nil {
				return fmt.Errorf("%s: %w", kv, err)
			}
			e.env = append(e.env, strings.TrimSpace(name)+"="+val)
		}
	}
	return nil
}

func home() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

// turn runs one exec turn on prompt.
func (e execArgs) turn(prompt string, out io.Writer) error {
	h := home()
	sessions := filepath.Join(h, "sessions", "2026", "10", "16")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		return err
	}
	sid := e.resume
	if sid == "" {
		n, _ := os.ReadDir(sessions)
		sid = fmt.Sprintf("0199fake-%04d", len(n)+1)
	}
	if err := appendFile(filepath.Join(h, "fakecodex.log"), fmt.Sprintf("--- turn %s\n%s\n", sid, prompt)); err != nil {
		return err
	}
	emit := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(out, string(b))
	}
	emit(map[string]any{"type": "thread.started", "thread_id": sid})
	emit(map[string]any{"type": "turn.started"})
	reply := "fakecodex heard: " + firstLine(prompt)
	tokens := int64(1000)
	for i, line := range strings.Split(prompt, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) >= 2 && f[0] == "TOKENS":
			if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				tokens = n
			}
		case len(f) >= 2 && f[0] == "CALL":
			tool := f[1]
			args := map[string]any{}
			if raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "CALL "+tool)); raw != "" {
				if err := json.Unmarshal([]byte(raw), &args); err != nil {
					return fmt.Errorf("CALL %s args: %w", tool, err)
				}
			}
			id := fmt.Sprintf("item_%d", i)
			item := map[string]any{"id": id, "type": "mcp_tool_call", "server": "saddle", "tool": tool, "arguments": args, "status": "in_progress"}
			emit(map[string]any{"type": "item.started", "item": item})
			text, err := e.call(tool, args)
			if err != nil {
				text = "error: " + err.Error()
			}
			item["status"] = "completed"
			emit(map[string]any{"type": "item.completed", "item": item})
			reply = fmt.Sprintf("called %s: %s", tool, oneLine(text, 300))
		}
	}
	model := e.model
	if model == "" {
		model = Model
	}
	if err := e.rollout(sessions, sid, model, tokens); err != nil {
		return err
	}
	emit(map[string]any{"type": "item.completed", "item": map[string]any{"id": "item_reply", "type": "agent_message", "text": reply}})
	emit(map[string]any{"type": "turn.completed", "usage": map[string]any{"input_tokens": tokens, "output_tokens": 10}})
	return nil
}

// call calls tool on the saddle MCP server the -c overrides name.
func (e execArgs) call(tool string, args map[string]any) (string, error) {
	if e.cmd == "" {
		return "", fmt.Errorf("no mcp_servers.saddle configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.Command(e.cmd, e.args...)
	cmd.Env = append(os.Environ(), e.env...)
	c := mcp.NewClient(&mcp.Implementation{Name: "fakecodex", Version: "e2e"}, nil)
	s, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = s.Close() }()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", err
	}
	var text []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text = append(text, t.Text)
		}
	}
	if res.IsError {
		return "", fmt.Errorf("%s", strings.Join(text, " "))
	}
	return strings.Join(text, " "), nil
}

// rollout appends the turn to the session's rollout, writing its
// session_meta first when the session is new.
func (e execArgs) rollout(dir, sid, model string, tokens int64) error {
	p := filepath.Join(dir, "rollout-2026-10-16T09-00-00-"+sid+".jsonl")
	var b strings.Builder
	line := func(v map[string]any) {
		j, _ := json.Marshal(v)
		b.Write(j)
		b.WriteByte('\n')
	}
	if _, err := os.Stat(p); err != nil {
		wd, _ := os.Getwd()
		line(map[string]any{"type": "session_meta", "payload": map[string]any{"id": sid, "cwd": wd}})
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	line(map[string]any{"timestamp": now, "type": "turn_context", "payload": map[string]any{"model": model}})
	line(map[string]any{"timestamp": now, "type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{
		"last_token_usage":     map[string]any{"input_tokens": tokens, "output_tokens": 10, "total_tokens": tokens + 10},
		"model_context_window": Window,
	}}})
	return appendFile(p, b.String())
}

func appendFile(p, s string) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func firstLine(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		if t := strings.TrimSpace(sc.Text()); t != "" {
			return t
		}
	}
	return ""
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n]
	}
	return s
}
