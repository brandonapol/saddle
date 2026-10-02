package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// DefaultModel is the planner model used when none is configured.
const DefaultModel = "claude-opus-5-5"

// Doer sends HTTP requests; *http.Client satisfies it.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Anthropic is a Model backed by the Messages API. It forces a submit_plan
// tool call whose input schema is the plan schema, so the reply is structured.
type Anthropic struct {
	APIKey    string
	Endpoint  string // default https://api.anthropic.com/v1/messages
	Model     string // default DefaultModel
	MaxTokens int    // default 16000
	HTTP      Doer   // default an http.Client with a 10 minute timeout
}

const planTool = "submit_plan"

// Complete implements Model.
func (a *Anthropic) Complete(ctx context.Context, c Call) ([]byte, error) {
	endpoint, model, maxTokens, client := a.Endpoint, a.Model, a.MaxTokens, a.HTTP
	if endpoint == "" {
		endpoint = "https://api.anthropic.com/v1/messages"
	}
	if model == "" {
		model = DefaultModel
	}
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	type tool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"system":     c.System,
		"messages":   []map[string]string{{"role": "user", "content": c.User}},
		"tools":      []tool{{Name: planTool, Description: "Submit the task plan for the epic.", InputSchema: c.Schema}},
		"tool_choice": map[string]string{
			"type": "tool",
			"name": planTool,
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("planner: messages API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("planner: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("planner: messages API returned %d: %s", resp.StatusCode, clip(string(raw), 300))
	}
	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("planner: decode response: %w", err)
	}
	if out.StopReason == "max_tokens" {
		return nil, errors.New("planner: the plan hit max_tokens before it was complete")
	}
	for _, b := range out.Content {
		if b.Type == "tool_use" && b.Name == planTool {
			return b.Input, nil
		}
	}
	return nil, errors.New("planner: response had no submit_plan call")
}

// Runner runs a command with stdin and returns its stdout.
type Runner func(ctx context.Context, name string, args []string, stdin string) ([]byte, error)

// ClaudeCLI is a Model that runs the Claude Code CLI in print mode, so
// planning uses the same subscription as the agents. The schema goes in the
// prompt and the JSON object is pulled out of the reply.
type ClaudeCLI struct {
	Cmd   string // default "claude"
	Model string // passed to --model when set
	Run   Runner // default runs the command for real
}

// Complete implements Model.
func (c *ClaudeCLI) Complete(ctx context.Context, call Call) ([]byte, error) {
	name, run := c.Cmd, c.Run
	if name == "" {
		name = "claude"
	}
	if run == nil {
		run = execRunner
	}
	args := []string{"-p", "--output-format", "json"}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	prompt := call.System + "\n\n" + call.User +
		"\nReply with only one JSON object that matches this JSON schema, and nothing else:\n" + string(call.Schema) + "\n"
	raw, err := run(ctx, name, args, prompt)
	if err != nil {
		return nil, fmt.Errorf("planner: %s: %w", name, err)
	}
	var out struct {
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("planner: decode %s output: %w", name, err)
	}
	if out.IsError {
		return nil, fmt.Errorf("planner: %s: %s", name, clip(out.Result, 300))
	}
	start, end := strings.Index(out.Result, "{"), strings.LastIndex(out.Result, "}")
	if start < 0 || end < start {
		return nil, fmt.Errorf("planner: %s reply has no JSON object: %s", name, clip(out.Result, 300))
	}
	return []byte(out.Result[start : end+1]), nil
}

func execRunner(ctx context.Context, name string, args []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, clip(strings.TrimSpace(errb.String()), 300))
	}
	return out, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
