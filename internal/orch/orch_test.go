package orch

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		line string
		want []Event
	}{
		{`{"type":"system","subtype":"init","session_id":"s1"}`, []Event{{Kind: Init, SessionID: "s1"}}},
		{`{"type":"system","subtype":"hook_started"}`, nil},
		{`{"type":"system","subtype":"compact_boundary","session_id":"s1","compact_metadata":{"trigger":"manual","pre_tokens":150000}}`, []Event{{Kind: Compacted, Text: "manual", SessionID: "s1"}}},
		{`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"po"}}}`, []Event{{Kind: Delta, Text: "po"}}},
		{`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"x"}}}`, nil},
		{`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""},{"type":"text","text":"pong"}]}}`, []Event{{Kind: Text, Text: "pong"}}},
		{`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__saddle__spawn","input":{"title":"meter worker","prompt":"..."}}]}}`, []Event{{Kind: Tool, Text: "spawn meter worker"}}},
		{`{"type":"result","subtype":"success","result":"pong","total_cost_usd":0.04,"session_id":"s1"}`, []Event{{Kind: Result, SessionID: "s1", CostUSD: 0.04}}},
		{`{"type":"result","subtype":"error","is_error":true,"result":"boom"}`, []Event{{Kind: Error, Text: "boom"}}},
		{`not json`, nil},
	}
	for _, c := range cases {
		got := parse([]byte(c.line))
		if len(got) != len(c.want) {
			t.Fatalf("parse(%s) = %+v, want %+v", c.line, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parse(%s)[%d] = %+v, want %+v", c.line, i, got[i], c.want[i])
			}
		}
	}
}
