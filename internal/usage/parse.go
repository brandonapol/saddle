package usage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
		Type  string `json:"type"`
		Model string `json:"model"`
		Info  *struct {
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
	rec, ok, _, err := parseCodex(line)
	return rec, ok, err
}

// NewCodexParser returns a ParseCodex that remembers the model of the last
// turn_context line and labels the token counts after it with that model,
// so a session that switches models (/model) is split by model (#482).
// Records before any turn_context keep CodexModel.
func NewCodexParser() Parser {
	model := ""
	return func(line []byte) (Record, bool, error) {
		rec, ok, turn, err := parseCodex(line)
		if turn != "" {
			model = turn
		}
		if ok && model != "" {
			rec.Model = model
		}
		return rec, ok, err
	}
}

// parseCodex is ParseCodex that also returns a turn_context line's model.
func parseCodex(line []byte) (rec Record, ok bool, turnModel string, err error) {
	var l codexLine
	if err := json.Unmarshal(line, &l); err != nil {
		return Record{}, false, "", err
	}
	if l.Type == "turn_context" && l.Payload != nil {
		return Record{}, false, l.Payload.Model, nil
	}
	if l.Type != "event_msg" || l.Payload == nil || l.Payload.Type != "token_count" ||
		l.Payload.Info == nil || l.Payload.Info.Last == nil {
		return Record{}, false, "", nil
	}
	ts, err := parseTime(l.Timestamp)
	if err != nil {
		return Record{}, false, "", err
	}
	u := l.Payload.Info.Last
	id := ""
	if t := l.Payload.Info.Total; t != nil {
		id = strconv.FormatInt(t.Total, 10)
	}
	return Record{
		ID: id, Time: ts, Model: CodexModel,
		Tokens: Tokens{Input: u.Input - u.CachedInput, CacheRead: u.CachedInput, Output: u.Output},
	}, true, "", nil
}

// CodexRolloutModel reads the first turn context from a rollout. Token-count
// events do not repeat the model, so the collector uses this session metadata
// to price and label their records.
func CodexRolloutModel(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l struct {
			Type    string `json:"type"`
			Payload struct {
				Model string `json:"model"`
			} `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Type == "turn_context" && l.Payload.Model != "" {
			return l.Payload.Model, nil
		}
	}
	return "", sc.Err()
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

type grokContextLine struct {
	Timestamp int64 `json:"timestamp"`
	Params    *struct {
		Update *struct {
			SessionUpdate string `json:"sessionUpdate"`
			TokensAfter   int64  `json:"tokens_after"`
		} `json:"update"`
		Meta *struct {
			TotalTokens int64  `json:"totalTokens"`
			EventID     string `json:"eventId"`
		} `json:"_meta"`
	} `json:"params"`
}

// ParseGrokContext reads one Grok updates.jsonl event. Grok's usage.json
// totals every model call in a user turn; _meta.totalTokens is the prompt size
// of this model call and therefore the useful context-window reading. An
// auto_compact_completed event's tokens_after is the context right after a
// compaction, before the next model call reports one.
func ParseGrokContext(line []byte) (Record, bool, error) {
	var l grokContextLine
	if err := json.Unmarshal(line, &l); err != nil {
		return Record{}, false, err
	}
	if l.Params == nil {
		return Record{}, false, nil
	}
	var tokens int64
	var id string
	if m := l.Params.Meta; m != nil {
		tokens, id = m.TotalTokens, m.EventID
	}
	if u := l.Params.Update; tokens <= 0 && u != nil && u.SessionUpdate == "auto_compact_completed" {
		tokens = u.TokensAfter
	}
	if tokens <= 0 {
		return Record{}, false, nil
	}
	var ts time.Time
	if l.Timestamp > 0 {
		ts = time.Unix(l.Timestamp, 0)
	}
	return Record{ID: id, Time: ts, Model: Grok, Tokens: Tokens{Input: tokens}}, true, nil
}

// ContextReading is one transcript line's view of a session's context: the
// prompt of its latest model call against the window the harness reported.
// Zero fields are unknown.
type ContextReading struct {
	Time   time.Time
	Model  string
	Prompt int64
	Window int64
}

// ParseCodexContext reads one Codex rollout line for a context reading. A
// token_count event's last_token_usage is the latest model call, whose
// input_tokens (cached ones included) is the prompt, and model_context_window
// is the window Codex itself sizes against. A turn_context line yields only
// its model. ok is false for every other line.
func ParseCodexContext(line []byte) (ContextReading, bool, error) {
	var l struct {
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
		Payload   *struct {
			Type  string `json:"type"`
			Model string `json:"model"`
			Info  *struct {
				Last   *codexUsage `json:"last_token_usage"`
				Window int64       `json:"model_context_window"`
			} `json:"info"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &l); err != nil {
		return ContextReading{}, false, err
	}
	p := l.Payload
	if p == nil {
		return ContextReading{}, false, nil
	}
	if l.Type == "turn_context" && p.Model != "" {
		return ContextReading{Model: p.Model}, true, nil
	}
	if l.Type != "event_msg" || p.Type != "token_count" || p.Info == nil || p.Info.Last == nil || p.Info.Last.Input <= 0 {
		return ContextReading{}, false, nil
	}
	ts, err := parseTime(l.Timestamp)
	if err != nil {
		return ContextReading{}, false, err
	}
	return ContextReading{Time: ts, Prompt: p.Info.Last.Input, Window: p.Info.Window}, true, nil
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

type grokSnapshotUsage struct {
	Input         int64 `json:"inputTokens"`
	Output        int64 `json:"outputTokens"`
	CacheRead     int64 `json:"cachedReadTokens"`
	CacheCreation int64 `json:"cacheCreationTokens"`
	CostTicks     int64 `json:"costUsdTicks"`
}

// ParseGrokSnapshot reads Grok CLI's persisted usage.json. Completed turns
// are stable records, and per-model rows preserve mixed-model sessions.
func ParseGrokSnapshot(data []byte) ([]Record, error) {
	var f struct {
		Turns []struct {
			Turn       int                          `json:"turnNumber"`
			EndedAt    string                       `json:"endedAt"`
			Primary    string                       `json:"primaryModelId"`
			Input      int64                        `json:"inputTokens"`
			Output     int64                        `json:"outputTokens"`
			CacheRead  int64                        `json:"cachedReadTokens"`
			CacheWrite int64                        `json:"cacheCreationTokens"`
			CostTicks  int64                        `json:"costUsdTicks"`
			Models     map[string]grokSnapshotUsage `json:"modelUsage"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	var out []Record
	for _, turn := range f.Turns {
		if turn.EndedAt == "" {
			continue
		}
		ts, err := parseTime(turn.EndedAt)
		if err != nil {
			return nil, err
		}
		models := make([]string, 0, len(turn.Models))
		for model := range turn.Models {
			models = append(models, model)
		}
		if len(models) == 0 {
			model := turn.Primary
			if model == "" {
				model = Grok
			}
			models = append(models, model)
			turn.Models = map[string]grokSnapshotUsage{model: {
				Input: turn.Input, Output: turn.Output, CacheRead: turn.CacheRead,
				CacheCreation: turn.CacheWrite, CostTicks: turn.CostTicks,
			}}
		}
		sort.Strings(models)
		for _, model := range models {
			u := turn.Models[model]
			// Grok's persisted inputTokens includes both cache classes;
			// normalize it to the same disjoint buckets the other parsers use.
			input := u.Input - u.CacheRead - u.CacheCreation
			if input < 0 {
				input = 0
			}
			out = append(out, Record{
				ID: fmt.Sprintf("%d:%s:%s", turn.Turn, model, turn.EndedAt), Time: ts, Model: model,
				Tokens:  Tokens{Input: input, Output: u.Output, CacheRead: u.CacheRead, CacheCreation: u.CacheCreation},
				CostUSD: float64(u.CostTicks) / 1e10,
			})
		}
	}
	return out, nil
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
