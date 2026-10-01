package usage

import (
	"encoding/json"
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
	var ts time.Time
	if l.Timestamp != "" {
		t, err := time.Parse(time.RFC3339Nano, l.Timestamp)
		if err != nil {
			return Record{}, false, err
		}
		ts = t
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

// ParseCodex is a stub: Codex usage is not collected yet.
func ParseCodex([]byte) (Record, bool, error) { return Record{}, false, nil }

// ParseGrok is a stub: Grok usage is not collected yet.
func ParseGrok([]byte) (Record, bool, error) { return Record{}, false, nil }
