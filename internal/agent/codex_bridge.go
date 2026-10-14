package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/usage"
)

// codexSpec is the headless invocation saddle's codex bridge replays once per
// user message: `codex exec --json <args> -` for a new session, and
// `codex exec --json <args> resume <id> -` after that (#506).
type codexSpec struct {
	Cmd    string   `json:"cmd"`
	Args   []string `json:"args"` // exec options valid on both exec and exec resume
	Dir    string   `json:"dir"`
	Env    []string `json:"env"`
	Brief  string   `json:"brief"` // codex has no system-prompt flag: it opens each new session
	Model  string   `json:"model,omitempty"`
	Resume string   `json:"resume,omitempty"`
}

// codexCompactPrompt asks the old session for the summary a fresh one starts from.
const codexCompactPrompt = "[saddle] This session is being compacted: a fresh session will replace it and see only your reply. " +
	"Reply with a short summary of where things stand: the owner's open requests, the tasks in flight and anything you promised to do. " +
	"Reply with the summary only."

func (l Launch) headlessCodex(resume string) (*exec.Cmd, error) {
	if err := l.record(usage.Codex); err != nil {
		return nil, err
	}
	env := l.env()
	spec := codexSpec{
		Cmd: l.cmdOr("codex"), Args: l.codexExecArgs(), Dir: l.Dir, Brief: l.Brief,
		Model: l.Model, Resume: resume, Env: envList(env),
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(l.RunDir, "codex.json"), b, 0o644); err != nil {
		return nil, err
	}
	cmd := exec.Command(l.Bin, "codex-bridge", l.RunDir)
	cmd.Dir = l.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Env = append(cmd.Env, "PATH="+l.envPath())
	return cmd, nil
}

// codexExecArgs are the options each headless codex turn takes: the saddle
// MCP server through -c overrides, as interactive codex workers get it, the
// model and the sandbox. exec resume takes no --sandbox, so the sandbox is a
// config override too.
func (l Launch) codexExecArgs() []string {
	env := l.env()
	args := []string{
		"-c", "mcp_servers.saddle.command=" + strconv.Quote(l.Bin),
		"-c", `mcp_servers.saddle.args=["mcp"]`,
		"-c", fmt.Sprintf("mcp_servers.saddle.env={SADDLE_ROOT=%s,SADDLE_TASK=%s}",
			strconv.Quote(env["SADDLE_ROOT"]), strconv.Quote(env["SADDLE_TASK"])),
	}
	if l.Model != "" {
		args = append(args, "-m", l.Model)
	}
	extra := codexExecExtra(l.Args)
	if !setsSandbox(extra) {
		switch l.Mode {
		case "acceptEdits":
			args = append(args, "-c", `sandbox_mode="workspace-write"`)
		case "plan", "default":
			args = append(args, "-c", `sandbox_mode="read-only"`)
		default:
			// The orchestrator runs saddle and git against the main checkout.
			args = append(args, "--dangerously-bypass-approvals-and-sandbox")
		}
	}
	return append(args, extra...)
}

// codexExecExtra keeps the [adapters.codex] args exec and exec resume both
// accept: config overrides and feature toggles. Interactive-only flags such
// as --search or -a would make every headless turn fail.
func codexExecExtra(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, _, inline := strings.Cut(a, "=")
		switch flag {
		case "-c", "--config", "--enable", "--disable":
			if inline {
				out = append(out, a)
			} else if i+1 < len(args) {
				out = append(out, a, args[i+1])
				i++
			}
		case "--dangerously-bypass-approvals-and-sandbox", "--yolo":
			out = append(out, "--dangerously-bypass-approvals-and-sandbox")
		}
	}
	return out
}

// setsSandbox reports whether exec args already choose the sandbox.
func setsSandbox(args []string) bool {
	for i, a := range args {
		if a == "--dangerously-bypass-approvals-and-sandbox" {
			return true
		}
		v := a
		if (a == "-c" || a == "--config") && i+1 < len(args) {
			v = args[i+1]
		}
		v = strings.TrimPrefix(strings.TrimPrefix(v, "--config="), "-c=")
		if strings.HasPrefix(strings.TrimSpace(v), "sandbox_mode") {
			return true
		}
	}
	return false
}

// RunCodexBridge reads Claude-style stream-json user lines from r and, for
// each one, runs a headless `codex exec --json` turn, resuming the session
// the first turn started so context carries across turns. Codex's JSONL
// events are translated to the stream-json the orchestrator parses. A
// "/compact" line asks the session for a summary and moves to a fresh
// session that starts from it, since exec has no compaction of its own.
func RunCodexBridge(runDir string, r io.Reader, w io.Writer) error {
	b, err := os.ReadFile(filepath.Join(runDir, "codex.json"))
	if err != nil {
		return err
	}
	var spec codexSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return err
	}
	if spec.Cmd == "" {
		spec.Cmd = "codex"
	}
	br := &codexBridge{spec: spec, w: w, resume: spec.Resume}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		text, ok := userText(sc.Bytes())
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		if keep, ok := compactRequest(text); ok {
			br.compact(keep)
			continue
		}
		br.turn(text)
	}
	return sc.Err()
}

type codexBridge struct {
	spec    codexSpec
	w       io.Writer
	resume  string
	summary string // carried into the next fresh session after a compaction
}

// turn runs one user message, starting a fresh session when the stored one
// can't be resumed.
func (br *codexBridge) turn(text string) {
	res := br.run(br.prompt(text), br.resume, true)
	if res.err != nil && br.resume != "" && res.session == "" {
		// A session id from another adapter, or a deleted rollout, should
		// not wedge the chat. One retry starts fresh.
		res = br.run(br.prompt(text), "", true)
	}
	if res.session != "" {
		if res.session != br.resume {
			br.summary = ""
		}
		br.resume = res.session
	}
	if res.err != nil {
		writeResult(br.w, true, res.err.Error(), br.resume)
	}
}

// prompt is text, preceded in a new session by the brief and any summary of
// the session it replaces.
func (br *codexBridge) prompt(text string) string {
	if br.resume != "" {
		return text
	}
	var b strings.Builder
	if s := strings.TrimSpace(br.spec.Brief); s != "" {
		b.WriteString(s + "\n\n")
	}
	if br.summary != "" {
		b.WriteString("[saddle] This session continues one that was compacted. Its summary:\n" + br.summary + "\n\n")
	}
	b.WriteString(text)
	return b.String()
}

// compact asks the session for a summary, then starts a fresh session from
// it and reports the boundary, as Claude Code does for /compact.
func (br *codexBridge) compact(keep string) {
	if br.resume == "" {
		writeResult(br.w, false, "Nothing to compact yet.", "")
		return
	}
	ask := codexCompactPrompt
	if keep != "" {
		ask += " Keep: " + keep
	}
	old := br.run(ask, br.resume, false)
	if old.err != nil {
		writeResult(br.w, true, "compact: "+old.err.Error(), br.resume)
		return
	}
	br.summary = strings.TrimSpace(old.text)
	br.resume = ""
	fresh := br.run(br.prompt("[saddle] Reply with the word ready, then wait for the owner's next message."), "", false)
	if fresh.err != nil {
		writeResult(br.w, true, "compact: "+fresh.err.Error(), "")
		return
	}
	br.resume = fresh.session
	for _, l := range []map[string]any{
		{"type": "system", "subtype": "init", "session_id": fresh.session, "model": br.spec.Model},
		{"type": "system", "subtype": "compact_boundary", "session_id": fresh.session,
			"compact_metadata": map[string]any{"trigger": "manual"}},
		{"type": "result", "subtype": "success", "is_error": false, "result": "Compacted.", "session_id": fresh.session},
	} {
		if writeLine(br.w, l) != nil {
			return
		}
	}
}

// compactRequest reports whether text is a /compact command, and its
// instruction.
func compactRequest(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if t != "/compact" && !strings.HasPrefix(t, "/compact ") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(t, "/compact")), true
}

type codexTurn struct {
	session string // the thread codex ran the turn on; "" when it never started
	text    string // the last agent message
	err     error
}

// run runs one codex exec turn. With forward set, its events go to the
// orchestrator as stream-json.
func (br *codexBridge) run(prompt, resume string, forward bool) codexTurn {
	spec := br.spec
	args := []string{"exec", "--json", "--skip-git-repo-check"}
	args = append(args, spec.Args...)
	if resume != "" {
		args = append(args, "resume", resume)
	}
	args = append(args, "-")
	cmd := exec.Command(spec.Cmd, args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Stdin = strings.NewReader(prompt)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return codexTurn{err: err}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return codexTurn{err: err}
	}
	if err := cmd.Start(); err != nil {
		return codexTurn{err: err}
	}
	errCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(stderr)
		if len(b) > 4096 {
			b = b[len(b)-4096:]
		}
		errCh <- strings.TrimSpace(string(b))
	}()
	tr := codexTranslator{model: spec.Model}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		for _, out := range tr.translate(sc.Bytes()) {
			if !forward {
				continue
			}
			if err := writeLine(br.w, out); err != nil {
				_ = cmd.Process.Kill()
				return codexTurn{session: tr.session, err: err}
			}
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	tail := <-errCh
	res := codexTurn{session: tr.session, text: tr.last}
	switch {
	case scanErr != nil:
		res.err = scanErr
	case tr.failed != "":
		res.err = fmt.Errorf("%s", tr.failed)
	case !tr.done:
		if tail == "" && tr.lastErr != "" {
			tail = tr.lastErr
		}
		if tail == "" && waitErr != nil {
			tail = waitErr.Error()
		}
		if tail == "" {
			tail = "codex exited without finishing the turn"
		}
		res.err = fmt.Errorf("%s", tail)
	}
	return res
}

// codexTranslator turns `codex exec --json` events into Claude stream-json
// lines: thread.started is the init line, agent messages are assistant text,
// commands, MCP calls, file changes and searches are tool_use blocks, and
// turn.completed or turn.failed is the result.
type codexTranslator struct {
	model   string
	session string
	last    string          // the last agent message
	started map[string]bool // items already shown as tool_use
	done    bool            // turn.completed
	failed  string          // turn.failed's message
	lastErr string          // the last top-level error event
}

type codexEvent struct {
	Type     string    `json:"type"`
	ThreadID string    `json:"thread_id"`
	Message  string    `json:"message"`
	Item     codexItem `json:"item"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage json.RawMessage `json:"usage"`
}

type codexItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Command   string          `json:"command"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Query     string          `json:"query"`
	Changes   []struct {
		Path string `json:"path"`
	} `json:"changes"`
}

func (t *codexTranslator) translate(line []byte) []map[string]any {
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "thread.started":
		t.session = ev.ThreadID
		return []map[string]any{{"type": "system", "subtype": "init", "session_id": ev.ThreadID, "model": t.model}}
	case "item.started", "item.updated", "item.completed":
		return t.item(ev.Type, ev.Item)
	case "turn.completed":
		t.done = true
		out := map[string]any{
			"type": "result", "subtype": "success", "is_error": false,
			"result": t.last, "session_id": t.session,
		}
		if len(ev.Usage) > 0 {
			out["usage"] = ev.Usage
		}
		return []map[string]any{out}
	case "turn.failed":
		t.failed = ev.Error.Message
		if t.failed == "" {
			t.failed = t.lastErr
		}
		if t.failed == "" {
			t.failed = "codex turn failed"
		}
		return nil // run reports it as the turn's error result
	case "error":
		t.lastErr = ev.Message
	}
	return nil
}

func (t *codexTranslator) item(kind string, it codexItem) []map[string]any {
	if it.Type == "agent_message" {
		if kind != "item.completed" || strings.TrimSpace(it.Text) == "" {
			return nil
		}
		t.last = it.Text
		return []map[string]any{assistant(map[string]any{"type": "text", "text": it.Text}, t.session)}
	}
	if t.started == nil {
		t.started = map[string]bool{}
	}
	if t.started[it.ID] {
		return nil
	}
	var name string
	var input any
	switch it.Type {
	case "command_execution":
		name, input = "Bash", map[string]any{"command": it.Command}
	case "mcp_tool_call":
		name, input = "mcp__"+it.Server+"__"+it.Tool, it.Arguments
		if len(it.Arguments) == 0 {
			input = map[string]any{}
		}
	case "file_change":
		if kind != "item.completed" {
			return nil // its paths arrive with the completed item
		}
		var paths []string
		for _, c := range it.Changes {
			paths = append(paths, c.Path)
		}
		name, input = "Edit", map[string]any{"file_path": strings.Join(paths, ", ")}
	case "web_search":
		name, input = "WebSearch", map[string]any{"query": it.Query}
	default:
		return nil // reasoning, todo lists
	}
	t.started[it.ID] = true
	return []map[string]any{assistant(map[string]any{"type": "tool_use", "id": it.ID, "name": name, "input": input}, t.session)}
}

func assistant(block map[string]any, session string) map[string]any {
	return map[string]any{
		"type": "assistant", "session_id": session,
		"message": map[string]any{"role": "assistant", "content": []any{block}},
	}
}

func writeLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
