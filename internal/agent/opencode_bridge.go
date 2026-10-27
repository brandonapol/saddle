package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/usage"
)

// openCodeSpec is the headless invocation saddle's opencode bridge replays
// once per user message: `opencode run --format json <args>` with the
// message on stdin, and `-s <id>` once a session exists (#745).
type openCodeSpec struct {
	Cmd    string   `json:"cmd"`
	Args   []string `json:"args"` // run options: the approval mode and the [adapters.opencode] args run takes
	Dir    string   `json:"dir"`
	Env    []string `json:"env"`
	Brief  string   `json:"brief"` // opens each new session's first message, as for codex
	Model  string   `json:"model,omitempty"`
	Resume string   `json:"resume,omitempty"`
	// Context is the ledger each step's tokens are appended to, in Claude
	// transcript form, so the orchestrator's context meter can read them.
	Context string `json:"context,omitempty"`
}

const (
	openCodeSpecFile = "opencode-orchestrator.json"
	// OpenCodeContextFile is the opencode orchestrator's context ledger in
	// its run dir (#745): one Claude-style assistant line per model call.
	OpenCodeContextFile = "opencode-context.jsonl"
	// OpenCodeContextModel names the ledger's records when no model is set.
	OpenCodeContextModel = "opencode"
)

func (l Launch) headlessOpenCode(resume string) (*exec.Cmd, error) {
	if err := l.record(usage.OpenCode); err != nil {
		return nil, err
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"mcp":     openCodeMCP(l),
	}
	if p := openCodePermission(l.Deny); len(p) > 0 {
		cfg["permission"] = p
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	conf := filepath.Join(l.RunDir, openCodeConfig)
	if err := os.WriteFile(conf, append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	env := l.env()
	spec := openCodeSpec{
		Cmd: l.cmdOr("opencode"), Args: append(openCodeApproval(l.Mode), openCodeRunExtra(l.Args)...),
		Dir: l.Dir, Brief: l.Brief, Model: l.Model, Resume: resume,
		Env:     append(envList(env), "OPENCODE_CONFIG="+conf, "OPENCODE_DISABLE_AUTOUPDATE=1"),
		Context: filepath.Join(l.RunDir, OpenCodeContextFile),
	}
	if b, err = json.MarshalIndent(spec, "", "  "); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(l.RunDir, openCodeSpecFile), b, 0o644); err != nil {
		return nil, err
	}
	cmd := exec.Command(l.Bin, "opencode-bridge", l.RunDir)
	cmd.Dir = l.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Env = append(cmd.Env, "PATH="+l.envPath())
	return cmd, nil
}

// openCodeMCP is the run-dir config's mcp table: the saddle server with the
// task's environment, as OPENCODE_CONFIG merges it over the user's own.
func openCodeMCP(l Launch) map[string]any {
	return map[string]any{"saddle": map[string]any{
		"type": "local", "command": []string{l.Bin, "mcp"}, "environment": l.env(), "enabled": true,
	}}
}

// openCodePermission turns the orchestrator's deny list (Claude tool names
// and Bash(cmd:*) rules) into opencode's permission config: edit covers
// opencode's write, edit and patch tools, task its subagents, and a Bash
// rule becomes a denied command pattern. Claude-only tools have no
// opencode counterpart and are left out. Verified with opencode 1.19.0: a
// denied edit leaves the model without its write tool, and a denied
// command fails the bash call with the rule, even under --auto.
func openCodePermission(deny []string) map[string]any {
	out := map[string]any{}
	bash := map[string]string{}
	for _, d := range deny {
		switch {
		case d == "Edit" || d == "Write" || d == "MultiEdit" || d == "NotebookEdit":
			out["edit"] = "deny"
		case d == "Agent":
			out["task"] = "deny"
		case strings.HasPrefix(d, "Bash(") && strings.HasSuffix(d, ")"):
			pat := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(d, "Bash("), ")"), ":*")
			bash[pat+"*"] = "deny"
		}
	}
	if len(bash) > 0 {
		out["bash"] = bash
	}
	return out
}

// openCodeRunExtra keeps the [adapters.opencode] args `opencode run` also
// takes. TUI-only flags such as --prompt would fail every headless turn.
func openCodeRunExtra(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, _, inline := strings.Cut(a, "=")
		switch flag {
		case "--agent", "--variant", "--log-level":
			if inline {
				out = append(out, a)
			} else if i+1 < len(args) {
				out = append(out, a, args[i+1])
				i++
			}
		case "--pure", "--auto":
			out = append(out, a)
		}
	}
	return out
}

// RunOpenCodeBridge reads Claude-style stream-json user lines from r and,
// for each one, runs a headless `opencode run --format json` turn, resuming
// the session the first turn started with -s, so context carries across
// turns. opencode's JSONL events become the stream-json the orchestrator
// parses. run has no /compact, so a "/compact" line asks the session for a
// summary and moves to a fresh session started from it, as for codex.
func RunOpenCodeBridge(runDir string, r io.Reader, w io.Writer) error {
	b, err := os.ReadFile(filepath.Join(runDir, openCodeSpecFile))
	if err != nil {
		return err
	}
	var spec openCodeSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return err
	}
	if spec.Cmd == "" {
		spec.Cmd = "opencode"
	}
	or := openCodeRunner{spec: spec, w: w}
	br := &turnBridge{brief: spec.Brief, model: spec.Model, w: w, resume: spec.Resume, run: or.run}
	return br.serve(r)
}

type openCodeRunner struct {
	spec openCodeSpec
	w    io.Writer
}

// run runs one opencode run turn. With forward set, its events go to the
// orchestrator as stream-json. Every turn's token use goes to the context
// ledger.
func (or openCodeRunner) run(prompt, resume string, forward bool) bridgeTurn {
	spec := or.spec
	args := []string{"run", "--format", "json"}
	if spec.Model != "" {
		args = append(args, "-m", spec.Model)
	}
	if resume != "" {
		args = append(args, "-s", resume)
	}
	args = append(args, spec.Args...)
	cmd := exec.Command(spec.Cmd, args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	// opencode run appends piped stdin to the message, which keeps a long
	// brief off the command line.
	cmd.Stdin = strings.NewReader(prompt)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return bridgeTurn{err: err}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return bridgeTurn{err: err}
	}
	if err := cmd.Start(); err != nil {
		return bridgeTurn{err: err}
	}
	errCh := stderrTail(stderr)
	tr := openCodeTranslator{model: spec.Model}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		outs, step := tr.translate(sc.Bytes())
		if step != nil && spec.Context != "" {
			_ = appendContext(spec.Context, *step)
		}
		for _, out := range outs {
			if !forward {
				continue
			}
			if err := writeLine(or.w, out); err != nil {
				_ = cmd.Process.Kill()
				return bridgeTurn{session: tr.session, err: err}
			}
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	tail := ansiRe.ReplaceAllString(<-errCh, "")
	res := bridgeTurn{session: tr.session, text: tr.last}
	switch {
	case scanErr != nil:
		res.err = scanErr
	case tr.failed != "":
		res.err = fmt.Errorf("%s", openCodeQuotaText(tr.failed, tr.failedBody))
	case waitErr != nil:
		if tail == "" {
			tail = waitErr.Error()
		}
		res.err = fmt.Errorf("%s", openCodeQuotaText(tail, ""))
	case forward:
		out := map[string]any{
			"type": "result", "subtype": "success", "is_error": false,
			"result": tr.last, "session_id": tr.session,
		}
		if tr.step != nil {
			out["usage"] = tr.step.claudeUsage()
		}
		if err := writeLine(or.w, out); err != nil {
			res.err = err
		}
	}
	return res
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// openCodeQuotaText is a failed turn's error as the orchestrator's failover
// reads it. opencode's limit wordings ("Insufficient balance", or Zen's
// bare "Rate limit exceeded" with a quota type in the body) are not all ones
// usage.DetectLimitBanner knows, so a limit is labelled as one, with its
// reset when opencode names it.
func openCodeQuotaText(msg, body string) string {
	msg = strings.TrimSpace(msg)
	l, ok := usage.OpenCodeErrorLimit(msg, body)
	if !ok {
		return msg
	}
	b, banner := usage.DetectLimitBanner(msg)
	if !banner {
		msg = "opencode quota exceeded: " + msg
	}
	if b.Resets == "" && l.ResetIn > 0 {
		msg += " It will reset in " + resetWords(l.ResetIn) + "."
	}
	return msg
}

// resetWords writes d the way the reset parser reads it: "2 hours 5 minutes".
func resetWords(d time.Duration) string {
	d = d.Round(time.Minute)
	var parts []string
	if h := int(d.Hours()); h > 0 {
		parts = append(parts, fmt.Sprintf("%d hours", h))
	}
	if m := int(d.Minutes()) % 60; m > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%d minutes", max(m, 1)))
	}
	return strings.Join(parts, " ")
}

// openCodeStep is one model call's tokens from a step_finish event.
type openCodeStep struct {
	ID     string
	Time   time.Time
	Model  string
	Tokens struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	}
}

func (s openCodeStep) claudeUsage() map[string]any {
	return map[string]any{
		"input_tokens": s.Tokens.Input, "output_tokens": s.Tokens.Output + s.Tokens.Reasoning,
		"cache_read_input_tokens": s.Tokens.Cache.Read, "cache_creation_input_tokens": s.Tokens.Cache.Write,
	}
}

// appendContext writes s to the ledger as a Claude transcript line, which
// usage.ParseClaude reads.
func appendContext(path string, s openCodeStep) error {
	b, err := json.Marshal(map[string]any{
		"type": "assistant", "timestamp": s.Time.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"id": s.ID, "model": s.Model, "usage": s.claudeUsage()},
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// openCodeTranslator turns `opencode run --format json` events into Claude
// stream-json lines. As captured from opencode 1.19.0: every event carries
// sessionID, the first is the init line; text parts are assistant text;
// tool_use parts are tool_use blocks; step_finish carries a model call's
// tokens; error is the turn's failure. The result line is written when the
// process exits cleanly, since run prints no turn-end event of its own.
type openCodeTranslator struct {
	model      string
	session    string
	last       string          // the last text part
	seen       map[string]bool // tool calls already shown
	step       *openCodeStep   // the last step_finish
	failed     string          // the error event's message
	failedBody string          // the provider's response body, when the error has one
}

type openCodeEvent struct {
	Type      string `json:"type"`
	Timestamp int64  `json:"timestamp"`
	SessionID string `json:"sessionID"`
	Part      struct {
		ID     string `json:"id"`
		Text   string `json:"text"`
		Tool   string `json:"tool"`
		CallID string `json:"callID"`
		State  struct {
			Input json.RawMessage `json:"input"`
		} `json:"state"`
		Reason string          `json:"reason"`
		Tokens json.RawMessage `json:"tokens"`
	} `json:"part"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			Message      string `json:"message"`
			ResponseBody string `json:"responseBody"`
		} `json:"data"`
	} `json:"error"`
}

func (t *openCodeTranslator) translate(line []byte) ([]map[string]any, *openCodeStep) {
	var ev openCodeEvent
	if json.Unmarshal(line, &ev) != nil || ev.Type == "" {
		return nil, nil
	}
	var out []map[string]any
	if ev.SessionID != "" && t.session == "" {
		t.session = ev.SessionID
		out = append(out, map[string]any{"type": "system", "subtype": "init", "session_id": t.session, "model": t.model})
	}
	switch ev.Type {
	case "text":
		if strings.TrimSpace(ev.Part.Text) == "" {
			break
		}
		t.last = ev.Part.Text
		out = append(out, assistant(map[string]any{"type": "text", "text": ev.Part.Text}, t.session))
	case "tool_use":
		id := ev.Part.CallID
		if id == "" {
			id = ev.Part.ID
		}
		if t.seen == nil {
			t.seen = map[string]bool{}
		}
		if t.seen[id] {
			break
		}
		t.seen[id] = true
		var input any = ev.Part.State.Input
		if len(ev.Part.State.Input) == 0 {
			input = map[string]any{}
		}
		out = append(out, assistant(map[string]any{"type": "tool_use", "id": id, "name": openCodeToolName(ev.Part.Tool), "input": input}, t.session))
	case "step_finish":
		s := openCodeStep{ID: ev.Part.ID, Time: time.UnixMilli(ev.Timestamp), Model: t.model}
		if s.Model == "" {
			s.Model = OpenCodeContextModel
		}
		if json.Unmarshal(ev.Part.Tokens, &s.Tokens) != nil {
			break
		}
		t.step = &s
		return out, &s
	case "error":
		t.failed = ev.Error.Data.Message
		if t.failed == "" {
			t.failed = ev.Error.Name
		}
		if t.failed == "" {
			t.failed = "opencode turn failed"
		}
		t.failedBody = ev.Error.Data.ResponseBody
	}
	return out, nil
}

// openCodeToolNames maps opencode's built-in tools to Claude Code's names,
// which the TUI shows.
var openCodeToolNames = map[string]string{
	"bash": "Bash", "read": "Read", "write": "Write", "edit": "Edit", "multiedit": "MultiEdit",
	"patch": "Edit", "apply_patch": "Edit", "glob": "Glob", "grep": "Grep", "list": "LS",
	"webfetch": "WebFetch", "websearch": "WebSearch", "todowrite": "TodoWrite", "todoread": "TodoRead",
	"task": "Agent",
}

// openCodeToolName is tool's Claude name. opencode names MCP tools
// <server>_<tool>, so saddle's are saddle_status and so on.
func openCodeToolName(tool string) string {
	if n, ok := openCodeToolNames[tool]; ok {
		return n
	}
	if rest, ok := strings.CutPrefix(tool, "saddle_"); ok {
		return "mcp__saddle__" + rest
	}
	return tool
}
