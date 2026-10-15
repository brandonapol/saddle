package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RunGrokBridge reads Claude-style stream-json user lines from r and, for each
// one, runs a headless `grok --prompt-file` turn. Stdout is grok's
// streaming-messages-json, which the orchestrator already understands.
// Sessions resume across turns. The process stays up until r hits EOF, the
// same shape as a long-lived Claude stream-json session.
func RunGrokBridge(runDir string, r io.Reader, w io.Writer) error {
	b, err := os.ReadFile(filepath.Join(runDir, "grok.json"))
	if err != nil {
		return err
	}
	var spec grokSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return err
	}
	if spec.Cmd == "" {
		spec.Cmd = "grok"
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	resume := spec.Resume
	for sc.Scan() {
		text, ok := userText(sc.Bytes())
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		next, err := runGrokTurn(spec, runDir, text, resume, w)
		if err != nil && resume != "" {
			// A session id from another harness, or a deleted session, should
			// not wedge the chat. One retry starts fresh.
			next, err = runGrokTurn(spec, runDir, text, "", w)
		}
		if err != nil {
			writeResult(w, true, err.Error(), resume)
			continue
		}
		if next != "" {
			resume = next
		}
	}
	return sc.Err()
}

func runGrokTurn(spec grokSpec, runDir, text, resume string, w io.Writer) (string, error) {
	prompt := filepath.Join(runDir, "turn-prompt.md")
	if err := os.WriteFile(prompt, []byte(text), 0o644); err != nil {
		return "", err
	}
	args := append([]string{}, spec.Args...)
	args = append(args, "--prompt-file", prompt)
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	cmd := exec.Command(spec.Cmd, args...)
	cmd.Dir = spec.Dir
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, spec.Env...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	errCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(stderr)
		if len(b) > 4096 {
			b = b[len(b)-4096:]
		}
		errCh <- strings.TrimSpace(string(b))
	}()
	session := ""
	sawResult := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	// A blank error result is held until grok exits, so its stderr can
	// stand in for the missing text (#263). Each turn still gets one result.
	var held []byte
	for sc.Scan() {
		line := sc.Bytes()
		if id := sessionFromLine(line); id != "" {
			session = id
		}
		if isResult(line) {
			sawResult = true
			if blankError(line) {
				held = append([]byte{}, line...)
				continue
			}
		}
		if _, err := w.Write(append(append([]byte{}, line...), '\n')); err != nil {
			_ = cmd.Process.Kill()
			return session, err
		}
	}
	scanErr := sc.Err()
	// Drain stderr before Wait: Wait closes the pipe and would drop the tail.
	tail := <-errCh
	waitErr := cmd.Wait()
	if held != nil {
		if tail != "" {
			writeResult(w, true, tail, session)
		} else if _, err := w.Write(append(held, '\n')); err != nil {
			return session, err
		}
	}
	if scanErr != nil {
		return session, scanErr
	}
	if waitErr != nil && !sawResult {
		if tail == "" {
			tail = waitErr.Error()
		}
		return session, fmt.Errorf("%s", tail)
	}
	return session, nil
}

func userText(line []byte) (string, bool) {
	var v struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &v) != nil || v.Type != "user" {
		return "", false
	}
	var s string
	if json.Unmarshal(v.Message.Content, &s) == nil {
		return s, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(v.Message.Content, &blocks) != nil {
		return "", false
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == "" || bl.Type == "text" {
			b.WriteString(bl.Text)
		}
	}
	return b.String(), true
}

func sessionFromLine(line []byte) string {
	var v struct {
		SessionID  string `json:"session_id"`
		SessionID2 string `json:"sessionId"`
	}
	if json.Unmarshal(line, &v) != nil {
		return ""
	}
	if v.SessionID != "" {
		return v.SessionID
	}
	return v.SessionID2
}

func isResult(line []byte) bool {
	var v struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(line, &v) == nil && v.Type == "result"
}

// blankError reports whether a result line is an error with no text in
// "result" or "errors".
func blankError(line []byte) bool {
	var v struct {
		IsError bool     `json:"is_error"`
		Result  string   `json:"result"`
		Errors  []string `json:"errors"`
	}
	if json.Unmarshal(line, &v) != nil || !v.IsError || strings.TrimSpace(v.Result) != "" {
		return false
	}
	for _, e := range v.Errors {
		if strings.TrimSpace(e) != "" {
			return false
		}
	}
	return true
}

func writeResult(w io.Writer, isErr bool, text, session string) {
	b, err := json.Marshal(map[string]any{
		"type": "result", "subtype": "error", "is_error": isErr,
		"result": text, "session_id": session,
	})
	if err != nil {
		return
	}
	_, _ = w.Write(append(b, '\n'))
}
