package orch

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		line string
		want []Event
	}{
		{`{"type":"system","subtype":"init","session_id":"s1"}`, []Event{{Kind: Init, SessionID: "s1"}}},
		{`{"type":"system","subtype":"init","session_id":"s1","slash_commands":["budget","compact","saddle:status"],"skills":["budget","saddle:status"]}`,
			[]Event{{Kind: Init, SessionID: "s1", Commands: []Command{{Name: "budget", Skill: true}, {Name: "compact"}, {Name: "saddle:status", Skill: true}}}}},
		{`{"type":"system","subtype":"commands_changed","commands":[{"name":"budget","description":"Check budget (user)","argumentHint":"[week]"}]}`,
			[]Event{{Kind: Commands, Commands: []Command{{Name: "budget", Description: "Check budget (user)", ArgHint: "[week]"}}}}},
		{`{"type":"control_response","response":{"subtype":"success","request_id":"saddle-init","response":{"commands":[{"name":"loop","description":"Run on an interval","argumentHint":"[interval] <prompt>"}]}}}`,
			[]Event{{Kind: Commands, Commands: []Command{{Name: "loop", Description: "Run on an interval", ArgHint: "[interval] <prompt>"}}}}},
		{`{"type":"control_response","response":{"subtype":"error","request_id":"saddle-init","error":"nope"}}`, nil},
		{`{"type":"system","subtype":"hook_started"}`, nil},
		{`{"type":"system","subtype":"compact_boundary","session_id":"s1","compact_metadata":{"trigger":"manual","pre_tokens":150000}}`, []Event{{Kind: Compacted, Text: "manual", SessionID: "s1"}}},
		{`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"po"}}}`, []Event{{Kind: Delta, Text: "po"}}},
		{`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"x"}}}`, nil},
		{`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""},{"type":"text","text":"pong"}]}}`, []Event{{Kind: Text, Text: "pong"}}},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__saddle__spawn","input":{"title":"meter worker","prompt":"..."}}]}}`, []Event{{Kind: Tool, Text: "spawn meter worker"}}},
		// A local command's output comes back as a user line (CC 2.1.293).
		{`{"type":"user","message":{"role":"user","content":"<local-command-stdout>Total cost: $0.00</local-command-stdout>"}}`, []Event{{Kind: Local, Text: "Total cost: $0.00"}}},
		{`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"<local-command-stdout>ok</local-command-stdout>"}]}}`, []Event{{Kind: Local, Text: "ok"}}},
		{`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]}}`, nil},
		{`{"type":"result","subtype":"success","result":"pong","total_cost_usd":0.04,"session_id":"s1"}`, []Event{{Kind: Result, Text: "pong", SessionID: "s1", CostUSD: 0.04}}},
		{`{"type":"result","subtype":"success","result":"","session_id":"s1","permission_denials":[{"tool_name":"Write","tool_use_id":"t1","tool_input":{}},{"tool_name":"Bash","tool_use_id":"t2","tool_input":{"command":"rm -rf x"}}]}`,
			[]Event{{Kind: Denied, Text: "Write, Bash rm -rf x"}, {Kind: Result, SessionID: "s1"}}},
		{`{"type":"result","subtype":"error","is_error":true,"result":"boom"}`, []Event{{Kind: Error, Text: "boom"}}},
		// grok's error result has no "result"; its text is in "errors" (#263).
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["Not signed in. Run grok login."],"session_id":""}`,
			[]Event{{Kind: Error, Text: "Not signed in. Run grok login."}}},
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"","errors":["a","b"]}`, []Event{{Kind: Error, Text: "a\nb"}}},
		// The init line names the model, which grok's config may leave unset (#263).
		{`{"type":"system","subtype":"init","session_id":"s1","model":"grok-4"}`, []Event{{Kind: Init, SessionID: "s1", Model: "grok-4"}}},
		// Retries while the API is down or overloaded (#262, CC 2.1.293).
		{`{"type":"system","subtype":"api_retry","attempt":3,"max_retries":10,"retry_delay_ms":2370,"error_status":null,"error":"unknown","session_id":"s1"}`,
			[]Event{{Kind: Retry, Text: "API error (unknown); retrying, attempt 3/10, next in 2s"}}},
		{`{"type":"system","subtype":"api_retry","attempt":1,"max_retries":10,"retry_delay_ms":500,"error_status":529,"error":"overloaded_error"}`,
			[]Event{{Kind: Retry, Text: "API error (529 overloaded_error); retrying, attempt 1/10, next in 1s"}}},
		// /clear starts a new conversation (#261, CC 2.1.293).
		{`{"type":"conversation_reset","new_conversation_id":"c2","trigger":"clear","session_id":"s1"}`, []Event{{Kind: Cleared}}},
		{`not json`, nil},
	}
	for _, c := range cases {
		got := parse([]byte(c.line))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parse(%s) = %+v, want %+v", c.line, got, c.want)
		}
	}
}

// TestMain lets the test binary stand in for claude: with ORCH_FAKE set it
// speaks just enough stream-json for Start and Probe.
func TestMain(m *testing.M) {
	switch os.Getenv("ORCH_FAKE") {
	case "":
		os.Exit(m.Run())
	case "hang":
		time.Sleep(time.Hour)
	}
	enc := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var l struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		_ = json.Unmarshal(sc.Bytes(), &l)
		switch os.Getenv("ORCH_FAKE") {
		case "turn":
			// A user message starts a turn that sits in a tool call until
			// an interrupt arrives, as a long Bash call would.
			if l.Type == "user" {
				_ = enc.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
					map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "sleep 600"}}}}})
			}
			if l.Type == "control_request" && l.Request.Subtype == "interrupt" {
				_ = enc.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": l.RequestID}})
				_ = enc.Encode(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": ""})
			}
			continue
		case "deaf":
			// Ignores control requests, like grok-bridge.
			continue
		}
		if l.Type == "control_request" && l.Request.Subtype == "initialize" {
			_ = enc.Encode(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": l.RequestID,
				"response": map[string]any{"commands": []any{
					map[string]any{"name": "budget", "description": "Check budget", "argumentHint": ""},
				}},
			}})
		}
	}
	os.Exit(0)
}

func fake(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "ORCH_FAKE="+mode)
	return cmd
}

// Start asks for the session's commands up front: real Claude Code only
// sends system/init after the first user message, and autocomplete needs
// the list before that.
func TestStartAsksForCommands(t *testing.T) {
	p, err := Start(fake(t, "init"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	select {
	case e := <-p.Events():
		if e.Kind != Commands || len(e.Commands) != 1 || e.Commands[0].Name != "budget" {
			t.Fatalf("first event = %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no commands event")
	}
	if p.Busy() {
		t.Fatal("asking for commands made the session busy")
	}
}

func TestProbe(t *testing.T) {
	cs, err := Probe(context.Background(), fake(t, "init"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name != "budget" || cs[0].Description != "Check budget" {
		t.Fatalf("Probe = %+v", cs)
	}
}

func TestProbeTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Probe(ctx, fake(t, "hang"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Probe err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Probe didn't stop at the deadline")
	}
}

func TestProbeReportsEarlyExit(t *testing.T) {
	_, err := Probe(context.Background(), exec.Command("sh", "-c", "echo not logged in >&2; exit 3"))
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("Probe err = %v", err)
	}
}

// next returns the next event that isn't a commands listing.
func next(t *testing.T, p *Proc) Event {
	t.Helper()
	for {
		select {
		case e := <-p.Events():
			if e.Kind != Commands {
				return e
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no event")
		}
	}
}

// An interrupt stops the turn mid tool call with a control request, keeps the
// process and session, and reports the turn as interrupted, not as an error.
func TestInterruptStopsTurnInToolCall(t *testing.T) {
	p, err := Start(fake(t, "turn"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Interrupt(); !errors.Is(err, ErrIdle) {
		t.Fatalf("Interrupt while idle = %v, want ErrIdle", err)
	}
	if err := p.Send("run the long thing"); err != nil {
		t.Fatal(err)
	}
	if e := next(t, p); e.Kind != Tool {
		t.Fatalf("event = %+v, want the tool call", e)
	}
	if err := p.Interrupt(); err != nil {
		t.Fatal(err)
	}
	if !p.Interrupting() {
		t.Fatal("Interrupting() is false right after Interrupt")
	}
	if e := next(t, p); e.Kind != Interrupted {
		t.Fatalf("event = %+v, want Interrupted", e)
	}
	if p.Busy() || p.Interrupting() {
		t.Fatalf("after the interrupt: busy=%v interrupting=%v", p.Busy(), p.Interrupting())
	}
	// The session lives on.
	if err := p.Send("again"); err != nil {
		t.Fatal(err)
	}
	if e := next(t, p); e.Kind != Tool {
		t.Fatalf("event after interrupt = %+v, want a new turn", e)
	}
}

// A process that ignores the control request gets SIGINT.
func TestInterruptFallsBackToSignal(t *testing.T) {
	defer func(d time.Duration) { interruptWait = d }(interruptWait)
	interruptWait = 200 * time.Millisecond
	p, err := Start(fake(t, "deaf"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_ = p.Send("hello")
	if err := p.Interrupt(); err != nil {
		t.Fatal(err)
	}
	if e := next(t, p); e.Kind != Exit {
		t.Fatalf("event = %+v, want Exit after SIGINT", e)
	}
}

// An error result ends the turn too; the session must not stay busy.
func TestErrorResultClearsBusy(t *testing.T) {
	p, err := Start(exec.Command("sh", "-c", `read l; read l; echo '{"type":"result","subtype":"error","is_error":true,"result":"API down"}'; sleep 5`))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	_ = p.Send("hi")
	if e := next(t, p); e.Kind != Error {
		t.Fatalf("event = %+v", e)
	}
	if p.Busy() {
		t.Fatal("an error result left the session busy")
	}
}
