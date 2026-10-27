// Package fakeopencode is a stand-in for opencode's headless mode, for
// journeys that run saddle's orchestrator on opencode (#745). It answers
// `opencode run --format json [-m provider/model] [-s <session>]` with the
// message on stdin the way opencode 1.19.0 does: one JSON event per line
// (step_start, tool_use, text, step_finish, error), each with its
// sessionID, and the saddle MCP server started from the config
// OPENCODE_CONFIG names, as opencode starts the servers in its config.
//
// A message line `CALL <tool> <json args>` calls that saddle tool over MCP.
// A line `TOKENS <n>` makes the turn's last step report an n-token prompt,
// so the context gauge and compact nudge can be driven. A line `QUOTA`
// fails the turn with OpenCode Zen's balance error. Sessions are files
// under <data>/opencode/fakeopencode; -s with an unknown id fails as
// opencode does. Every message is appended to <data>/opencode/fakeopencode.log,
// where <data> is $XDG_DATA_HOME, default ~/.local/share.
package fakeopencode

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

// Version is what the fake's --version prints.
const Version = "1.19.0"

// QuotaMessage is the error a QUOTA turn fails with: Zen's empty balance.
const QuotaMessage = "Insufficient balance. Manage your billing here: https://opencode.ai/workspace/billing"

// Main runs the fake on args and returns its exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if slices.Contains(args, "--version") || slices.Contains(args, "-v") {
		fmt.Fprintln(stdout, Version)
		return 0
	}
	if len(args) == 0 || args[0] != "run" {
		fmt.Fprintln(stderr, "fakeopencode: only run is faked")
		return 2
	}
	if slices.Contains(args, "--help") {
		fmt.Fprintln(stdout, "opencode run [message..]\n      --format  format: default (formatted) or json (raw JSON events)")
		return 0
	}
	r := parse(args[1:])
	if r.format != "json" {
		fmt.Fprintln(stderr, "fakeopencode: only --format json is faked")
		return 2
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "fakeopencode:", err)
		return 1
	}
	msg := strings.TrimSpace(strings.Join(append(r.message, string(b)), "\n"))
	code, err := r.turn(msg, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
	}
	return code
}

type runArgs struct {
	format  string
	session string
	model   string
	message []string
}

func parse(args []string) runArgs {
	var r runArgs
	for i := 0; i < len(args); i++ {
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a := args[i]; a {
		case "--format":
			r.format = next()
		case "-s", "--session":
			r.session = next()
		case "-m", "--model":
			r.model = next()
		case "--agent", "--variant", "--log-level", "--dir", "--title":
			next()
		default:
			if !strings.HasPrefix(a, "-") {
				r.message = append(r.message, a)
			}
		}
	}
	return r
}

// DataDir is where the fake keeps its log and sessions.
func DataDir() string {
	d := os.Getenv("XDG_DATA_HOME")
	if d == "" {
		h, _ := os.UserHomeDir()
		d = filepath.Join(h, ".local", "share")
	}
	return filepath.Join(d, "opencode")
}

// LogPath is the fake's message log.
func LogPath() string { return filepath.Join(DataDir(), "fakeopencode.log") }

// turn runs one turn on msg and returns the exit code.
func (r runArgs) turn(msg string, out io.Writer) (int, error) {
	sessions := filepath.Join(DataDir(), "fakeopencode")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		return 1, err
	}
	sid := r.session
	if sid != "" {
		if _, err := os.Stat(filepath.Join(sessions, sid)); err != nil {
			return 1, fmt.Errorf("Session not found")
		}
	} else {
		n, _ := os.ReadDir(sessions)
		sid = fmt.Sprintf("ses_fake%04d", len(n)+1)
		if err := os.WriteFile(filepath.Join(sessions, sid), nil, 0o644); err != nil {
			return 1, err
		}
	}
	if err := appendFile(LogPath(), fmt.Sprintf("--- turn %s\n%s\n", sid, msg)); err != nil {
		return 1, err
	}
	n := 0
	emit := func(typ string, v map[string]any) {
		n++
		ev := map[string]any{"type": typ, "timestamp": time.Now().UnixMilli(), "sessionID": sid}
		for k, x := range v {
			ev[k] = x
		}
		b, _ := json.Marshal(ev)
		fmt.Fprintln(out, string(b))
	}
	part := func(v map[string]any) map[string]any {
		v["id"] = fmt.Sprintf("prt_fake%d", n)
		v["sessionID"] = sid
		return map[string]any{"part": v}
	}
	emit("step_start", part(map[string]any{"type": "step-start"}))
	reply := "fakeopencode heard: " + firstLine(msg)
	tokens := int64(1000)
	for i, line := range strings.Split(msg, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 1 && f[0] == "QUOTA":
			emit("error", map[string]any{"error": map[string]any{"name": "APIError", "data": map[string]any{"message": QuotaMessage, "statusCode": 401}}})
			return 1, nil
		case len(f) >= 2 && f[0] == "TOKENS":
			if v, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				tokens = v
			}
		case len(f) >= 2 && f[0] == "CALL":
			tool := f[1]
			args := map[string]any{}
			if raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "CALL "+tool)); raw != "" {
				if err := json.Unmarshal([]byte(raw), &args); err != nil {
					return 1, fmt.Errorf("CALL %s args: %w", tool, err)
				}
			}
			text, err := r.call(tool, args)
			status := "completed"
			if err != nil {
				text, status = "error: "+err.Error(), "error"
			}
			emit("tool_use", part(map[string]any{"type": "tool", "tool": "saddle_" + tool, "callID": fmt.Sprintf("call_%d", i),
				"state": map[string]any{"status": status, "input": args, "output": text}}))
			emit("step_finish", part(map[string]any{"type": "step-finish", "reason": "tool-calls",
				"tokens": map[string]any{"input": 100, "output": 5, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}}}))
			emit("step_start", part(map[string]any{"type": "step-start"}))
			reply = fmt.Sprintf("called %s: %s", tool, oneLine(text, 300))
		}
	}
	emit("text", part(map[string]any{"type": "text", "text": reply}))
	// Most of a resumed session's prompt is cache, as opencode reports it.
	emit("step_finish", part(map[string]any{"type": "step-finish", "reason": "stop",
		"tokens": map[string]any{"input": 10, "output": 8, "reasoning": 2, "cache": map[string]any{"read": tokens - 10, "write": 0}}}))
	return 0, nil
}

// call calls tool on the saddle MCP server OPENCODE_CONFIG names.
func (r runArgs) call(tool string, args map[string]any) (string, error) {
	var cfg struct {
		MCP map[string]struct {
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
		} `json:"mcp"`
	}
	p := os.Getenv("OPENCODE_CONFIG")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("no OPENCODE_CONFIG: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	s, ok := cfg.MCP["saddle"]
	if !ok || len(s.Command) == 0 {
		return "", fmt.Errorf("no mcp.saddle in %s", p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.Command(s.Command[0], s.Command[1:]...)
	cmd.Env = os.Environ()
	for k, v := range s.Environment {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "fakeopencode", Version: "e2e"}, nil)
	sess, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = sess.Close() }()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
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
