package usage

import (
	"encoding/json"
	"strconv"
	"time"
)

// Parser turns one transcript line into a Record. ok is false for lines that
// are valid but carry no usage (user messages, tool results, ...). A non-nil
// error means the line is malformed.
type Parser func(line []byte) (rec Record, ok bool, err error)

// Agent kinds understood by NewCollector.
const (
	Claude = "claude"
	Codex  = "codex"
	Grok   = "grok"
)

// ParserFor returns the parser for an agent kind, or nil if unknown.
func ParserFor(agent string) Parser {
	switch agent {
	case Claude:
		return ParseClaude
	case Codex:
		return ParseCodex
	case Grok:
		return ParseGrok
	}
	return nil
}

type claudeLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ParseClaude parses a Claude Code transcript line. Assistant lines carry
// message.usage; everything else is skipped. Claude Code writes one line per
// content block of a response, all repeating the same message id and usage, so
// Record.ID lets the collector count each response once.
func ParseClaude(line []byte) (Record, bool, error) {
	var l claudeLine
	if err := json.Unmarshal(line, &l); err != nil {
		return Record{}, false, err
	}
	if l.Message == nil || l.Message.Usage == nil {
		return Record{}, false, nil
	}
	if l.Message.Model == "<synthetic>" {
		return Record{}, false, nil
	}
	ts, err := parseTime(l.Timestamp)
	if err != nil {
		return Record{}, false, err
	}
	u := l.Message.Usage
	return Record{
		ID:    l.Message.ID,
		Time:  ts,
		Model: l.Message.Model,
		Tokens: Tokens{
			Input: u.Input, Output: u.Output,
			CacheRead: u.CacheRead, CacheCreation: u.CacheCreation,
		},
	}, true, nil
}

type codexLine struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   *struct {
		Type string `json:"type"`
		Info *struct {
			Total *codexUsage `json:"total_token_usage"`
			Last  *codexUsage `json:"last_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

type codexUsage struct {
	Input       int64 `json:"input_tokens"`
	CachedInput int64 `json:"cached_input_tokens"`
	Output      int64 `json:"output_tokens"`
	Total       int64 `json:"total_tokens"`
}

// CodexModel labels Codex usage: token counts don't name the model, and a
// Parser sees one line at a time.
const CodexModel = "codex"

// ParseCodex parses a line of a Codex CLI rollout
// ($CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl). Each turn ends with a
// token_count event whose last_token_usage is that turn's usage; input_tokens
// includes the cached ones. Codex can repeat the event, so Record.ID is the
// running total, which only a repeat shares.
func ParseCodex(line []byte) (Record, bool, error) {
	var l codexLine
	if err := json.Unmarshal(line, &l); err != nil {
		return Record{}, false, err
	}
	if l.Type != "event_msg" || l.Payload == nil || l.Payload.Type != "token_count" ||
		l.Payload.Info == nil || l.Payload.Info.Last == nil {
		return Record{}, false, nil
	}
	ts, err := parseTime(l.Timestamp)
	if err != nil {
		return Record{}, false, err
	}
	u := l.Payload.Info.Last
	id := ""
	if t := l.Payload.Info.Total; t != nil {
		id = strconv.FormatInt(t.Total, 10)
	}
	return Record{
		ID: id, Time: ts, Model: CodexModel,
		Tokens: Tokens{Input: u.Input - u.CachedInput, CacheRead: u.CachedInput, Output: u.Output},
	}, true, nil
}

type grokLine struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Usage   *struct {
		Prompt     int64 `json:"prompt_tokens"`
		Completion int64 `json:"completion_tokens"`
		Details    *struct {
			Cached int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// ParseGrok reads Grok CLI output. Three shapes count:
// streaming-messages-json assistant lines (same as Claude), a native
// {"type":"usage",...} event, and an OpenAI-style completion tee'd from a
// one-shot run. Plain console text is skipped.
func ParseGrok(line []byte) (Record, bool, error) {
	if len(line) == 0 || line[0] != '{' {
		return Record{}, false, nil
	}
	rec, ok, err := ParseClaude(line)
	if err != nil {
		return Record{}, false, err
	}
	if ok {
		return rec, true, nil
	}
	var ev struct {
		Type      string `json:"type"`
		MessageID string `json:"messageId"`
		Usage     *struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return Record{}, false, err
	}
	if ev.Type == "usage" && ev.Usage != nil {
		u := ev.Usage
		return Record{
			ID: ev.MessageID,
			Tokens: Tokens{
				Input: u.Input, Output: u.Output,
				CacheRead: u.CacheRead, CacheCreation: u.CacheCreation,
			},
		}, true, nil
	}
	var l grokLine
	if err := json.Unmarshal(line, &l); err != nil {
		return Record{}, false, err
	}
	if l.Usage == nil {
		return Record{}, false, nil
	}
	var ts time.Time
	if l.Created > 0 {
		ts = time.Unix(l.Created, 0)
	}
	var cached int64
	if l.Usage.Details != nil {
		cached = l.Usage.Details.Cached
	}
	model := l.Model
	if model == "" {
		model = Grok
	}
	return Record{
		ID: l.ID, Time: ts, Model: model,
		Tokens: Tokens{Input: l.Usage.Prompt - cached, CacheRead: cached, Output: l.Usage.Completion},
	}, true, nil
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
