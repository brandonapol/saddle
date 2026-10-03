// Package orch runs the orchestrator: a headless Claude Code process speaking
// stream-json, whose conversation the TUI renders as chat.
package orch

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
)

// Event kinds.
const (
	Delta  = "delta"  // streamed assistant text
	Text   = "text"   // a finished assistant text block
	Tool   = "tool"   // the agent called a tool
	Result = "result" // the turn finished
	Init   = "init"   // session started; SessionID is set
	Error  = "error"
	Exit   = "exit" // the process ended
	// Compacted: the session's context was compacted; Text is the trigger
	// ("manual" or "auto").
	Compacted = "compacted"
)

type Event struct {
	Kind      string
	Text      string
	SessionID string
	CostUSD   float64
}

type Proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan Event
	busy   atomic.Bool
	mu     sync.Mutex
}

// Start launches cmd (built by agent.Launch.Headless) and begins streaming its events.
func Start(cmd *exec.Cmd) (*Proc, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Proc{cmd: cmd, stdin: stdin, events: make(chan Event, 256)}
	var tail strings.Builder
	var tailMu sync.Mutex
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			tailMu.Lock()
			tail.WriteString(sc.Text() + "\n")
			tailMu.Unlock()
		}
	}()
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			for _, e := range parse(sc.Bytes()) {
				if e.Kind == Result {
					p.busy.Store(false)
				}
				p.events <- e
			}
		}
		err := cmd.Wait()
		p.busy.Store(false)
		tailMu.Lock()
		msg := strings.TrimSpace(tail.String())
		tailMu.Unlock()
		if err != nil && msg == "" {
			msg = err.Error()
		}
		p.events <- Event{Kind: Exit, Text: msg}
		close(p.events)
	}()
	return p, nil
}

// Events streams everything the agent does. It is closed after the Exit event.
func (p *Proc) Events() <-chan Event { return p.events }

// Busy reports whether a turn is in progress.
func (p *Proc) Busy() bool { return p.busy.Load() }

// Send queues a user message. Messages sent mid-turn are handled after it.
func (p *Proc) Send(text string) error {
	b, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.busy.Store(true)
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// Close ends the session: stdin closes, and the process is killed if it lingers.
func (p *Proc) Close() {
	p.mu.Lock()
	_ = p.stdin.Close()
	p.mu.Unlock()
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

type line struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	CostUSD   float64         `json:"total_cost_usd"`
	Event     json.RawMessage `json:"event"`
	Compact   struct {
		Trigger string `json:"trigger"`
	} `json:"compact_metadata"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// parse turns one stream-json line into zero or more events.
func parse(b []byte) []Event {
	var l line
	if err := json.Unmarshal(b, &l); err != nil {
		return nil
	}
	switch l.Type {
	case "system":
		switch l.Subtype {
		case "init":
			return []Event{{Kind: Init, SessionID: l.SessionID}}
		case "compact_boundary":
			return []Event{{Kind: Compacted, Text: l.Compact.Trigger, SessionID: l.SessionID}}
		}
	case "stream_event":
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(l.Event, &ev) == nil && ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
			return []Event{{Kind: Delta, Text: ev.Delta.Text}}
		}
	case "assistant":
		var out []Event
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					out = append(out, Event{Kind: Text, Text: c.Text})
				}
			case "tool_use":
				out = append(out, Event{Kind: Tool, Text: describeTool(c.Name, c.Input)})
			}
		}
		return out
	case "result":
		e := Event{Kind: Result, SessionID: l.SessionID, CostUSD: l.CostUSD}
		if l.IsError {
			e.Kind, e.Text = Error, l.Result
		}
		return []Event{e}
	}
	return nil
}

// describeTool renders a tool call as one short line for the chat.
func describeTool(name string, input json.RawMessage) string {
	name = strings.TrimPrefix(name, "mcp__saddle__")
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := in[k]; ok {
				s := fmt.Sprint(v)
				if len(s) > 80 {
					s = s[:79] + "…"
				}
				return s
			}
		}
		return ""
	}
	detail := ""
	switch name {
	case "spawn":
		detail = pick("title")
	case "peek", "kill", "send_keys":
		detail = pick("task")
	case "message":
		detail = pick("task") + ": " + pick("message")
	case "ticket":
		detail = "#" + pick("number")
	case "Bash":
		detail = pick("command")
	case "Read", "Glob", "Grep":
		detail = pick("file_path", "pattern")
	}
	if detail == "" {
		return name
	}
	return name + " " + detail
}
