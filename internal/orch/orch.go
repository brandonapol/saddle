// Package orch runs the orchestrator: a headless Claude Code process speaking
// stream-json, whose conversation the TUI renders as chat.
package orch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	// Commands: the session's slash commands and skills changed; Commands
	// is set. Init carries them too when the init message lists them.
	Commands = "commands"
	// Local: a local slash command's output (/cost, /context…).
	Local = "local"
	// Denied: the turn's tool calls that permissions refused; Text names them.
	Denied = "denied"
)

// Event is one thing the session did. Result's Text is the turn's final
// text, which the assistant events usually showed already.
type Event struct {
	Kind      string
	Text      string
	SessionID string
	CostUSD   float64
	Commands  []Command
}

// Command is a slash command the session offers. Skill marks one backed by
// a skill (user, project, plugin or bundled).
type Command struct {
	Name        string
	Description string
	ArgHint     string
	Skill       bool
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
	// Claude Code sends system/init only after the first user message; the
	// initialize request gets the commands now, without a model call.
	// grok-bridge ignores it.
	_, _ = stdin.Write(initRequest)
	var tail strings.Builder
	var tailMu sync.Mutex
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
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
		<-stderrDone // Wait closes the pipe; drain it first or the tail can be lost
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

// initRequest asks a stream-json session for its commands, as the Agent
// SDK does on connect.
var initRequest = []byte(`{"type":"control_request","request_id":"saddle-init","request":{"subtype":"initialize"}}` + "\n")

// Probe starts cmd (a stream-json Claude Code process), asks for its
// commands and stops it. No model call is made. It gives up when ctx ends.
func Probe(ctx context.Context, cmd *exec.Cmd) ([]Command, error) {
	p, err := Start(cmd)
	if err != nil {
		return nil, err
	}
	defer func() {
		p.Close()
		for range p.events {
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case e := <-p.events:
			switch e.Kind {
			case Commands:
				return e.Commands, nil
			case Exit:
				if e.Text == "" {
					e.Text = "exited before listing its commands"
				}
				return nil, errors.New(e.Text)
			}
		}
	}
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
	Type          string          `json:"type"`
	Subtype       string          `json:"subtype"`
	SessionID     string          `json:"session_id"`
	Result        string          `json:"result"`
	IsError       bool            `json:"is_error"`
	CostUSD       float64         `json:"total_cost_usd"`
	Event         json.RawMessage `json:"event"`
	SlashCommands []string        `json:"slash_commands"`
	Skills        []string        `json:"skills"`
	Commands      []wireCommand   `json:"commands"`
	Denials       []struct {
		Tool  string          `json:"tool_name"`
		Input json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
	Response struct {
		Subtype  string `json:"subtype"`
		Response struct {
			Commands []wireCommand `json:"commands"`
		} `json:"response"`
	} `json:"response"`
	Compact struct {
		Trigger string `json:"trigger"`
	} `json:"compact_metadata"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type wireCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ArgHint     string `json:"argumentHint"`
}

type block struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// blocks reads message content, a string or a list of blocks.
func blocks(raw json.RawMessage) []block {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{{Type: "text", Text: s}}
	}
	var bs []block
	_ = json.Unmarshal(raw, &bs)
	return bs
}

func commands(ws []wireCommand) []Command {
	out := make([]Command, 0, len(ws))
	for _, w := range ws {
		out = append(out, Command{Name: w.Name, Description: w.Description, ArgHint: w.ArgHint})
	}
	return out
}

// localOutput is the text inside <local-command-stdout> (or -stderr).
func localOutput(s string) (string, bool) {
	for _, tag := range []string{"local-command-stdout", "local-command-stderr"} {
		open, end := "<"+tag+">", "</"+tag+">"
		if i := strings.Index(s, open); i >= 0 {
			rest := s[i+len(open):]
			if j := strings.Index(rest, end); j >= 0 {
				rest = rest[:j]
			}
			return strings.TrimSpace(rest), true
		}
	}
	return "", false
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
			e := Event{Kind: Init, SessionID: l.SessionID}
			skill := map[string]bool{}
			for _, s := range l.Skills {
				skill[s] = true
			}
			for _, n := range l.SlashCommands {
				e.Commands = append(e.Commands, Command{Name: n, Skill: skill[n]})
			}
			return []Event{e}
		case "commands_changed":
			return []Event{{Kind: Commands, Commands: commands(l.Commands)}}
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
	case "control_response":
		if l.Response.Subtype == "success" && l.Response.Response.Commands != nil {
			return []Event{{Kind: Commands, Commands: commands(l.Response.Response.Commands)}}
		}
	case "user":
		for _, c := range blocks(l.Message.Content) {
			if c.Type != "text" {
				continue
			}
			if out, ok := localOutput(c.Text); ok && out != "" {
				return []Event{{Kind: Local, Text: out}}
			}
		}
	case "assistant":
		var out []Event
		for _, c := range blocks(l.Message.Content) {
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
		e := Event{Kind: Result, Text: l.Result, SessionID: l.SessionID, CostUSD: l.CostUSD}
		if l.IsError {
			e = Event{Kind: Error, Text: l.Result}
		}
		var out []Event
		if len(l.Denials) > 0 {
			var names []string
			for _, d := range l.Denials {
				names = append(names, describeTool(d.Tool, d.Input))
			}
			out = append(out, Event{Kind: Denied, Text: strings.Join(names, ", ")})
		}
		return append(out, e)
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
