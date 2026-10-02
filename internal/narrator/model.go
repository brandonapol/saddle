package narrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/brandonapol/saddle/internal/usage"
)

// Model is the language model behind narration and answers. The default
// sends Messages API requests through Deps.HTTP.
type Model interface {
	Complete(ctx context.Context, r Request) (Response, error)
}

// Request is one prompt. The system prompt is always a cache breakpoint.
type Request struct {
	Model     string
	MaxTokens int
	System    string
	Blocks    []Block // the user turn, in order
}

// Block is part of the user turn.
type Block struct {
	Text  string
	Cache bool // a prompt-cache breakpoint ends here
}

// Response is the model's text and the tokens it cost.
type Response struct {
	Text   string
	Tokens usage.Tokens
}

// Messages API wire types (only the fields the narrator uses).

type cacheControl struct {
	Type string `json:"type"`
}

type textBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type message struct {
	Role    string      `json:"role"`
	Content []textBlock `json:"content"`
}

type messagesRequest struct {
	Model     string      `json:"model"`
	MaxTokens int         `json:"max_tokens"`
	System    []textBlock `json:"system"`
	Messages  []message   `json:"messages"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type apiUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type messagesResponse struct {
	Content []contentBlock `json:"content"`
	Usage   apiUsage       `json:"usage"`
}

// APIError is a non-2xx response from the Messages API.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("narrator: messages API returned %d: %s", e.Status, e.Body)
}

// apiModel calls the Messages API.
type apiModel struct {
	endpoint, key string
	http          Doer
}

func (m apiModel) Complete(ctx context.Context, r Request) (Response, error) {
	ephemeral := &cacheControl{Type: "ephemeral"}
	var content []textBlock
	for _, b := range r.Blocks {
		tb := textBlock{Type: "text", Text: b.Text}
		if b.Cache {
			tb.CacheControl = ephemeral
		}
		content = append(content, tb)
	}
	body, err := json.Marshal(messagesRequest{
		Model:     r.Model,
		MaxTokens: r.MaxTokens,
		System:    []textBlock{{Type: "text", Text: r.System, CacheControl: ephemeral}},
		Messages:  []message{{Role: "user", Content: content}},
	})
	if err != nil {
		return Response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", m.key)
	req.Header.Set("anthropic-version", apiVersion)

	resp, err := m.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("narrator: messages API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var buf strings.Builder
	if _, err := io.Copy(&buf, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return Response{}, fmt.Errorf("narrator: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Response{}, &APIError{Status: resp.StatusCode, Body: truncate(buf.String(), 300)}
	}
	var out messagesResponse
	if err := json.Unmarshal([]byte(buf.String()), &out); err != nil {
		return Response{}, fmt.Errorf("narrator: decode response: %w", err)
	}
	var text []string
	for _, c := range out.Content {
		if c.Type == "text" {
			text = append(text, c.Text)
		}
	}
	if len(text) == 0 {
		return Response{}, errors.New("narrator: response had no text")
	}
	return Response{Text: strings.Join(text, "\n"), Tokens: usage.Tokens{
		Input:         out.Usage.InputTokens,
		Output:        out.Usage.OutputTokens,
		CacheRead:     out.Usage.CacheReadInputTokens,
		CacheCreation: out.Usage.CacheCreationInputTokens,
	}}, nil
}
