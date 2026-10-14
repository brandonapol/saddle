// Package triage decides, cheaply, which agent events deserve the
// orchestrator's or the user's attention. It asks TypeSafe's Jev (a decision
// model that returns typed answers with calibrated confidence) instead of an LLM.
package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.typesafe.ai"

// Question is one typed question for Jev: "choice" (pick a key of Criteria,
// a map) or "noul" (true/false, optional Criteria with "true"/"false").
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Noul          float64            `json:"noul"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// InputUSDPerMillion is Jev's list price in USD per million input tokens.
// Output tokens are free. Source: docs.typesafe.ai/models, Jev 1.13 ($42
// per billion input tokens).
const InputUSDPerMillion = 0.042

// ApproxSpendUSD estimates what inputTokens cost at the list price.
func ApproxSpendUSD(inputTokens int64) float64 {
	return float64(inputTokens) / 1_000_000 * InputUSDPerMillion
}

// Call is one Jev request and how it turned out. Error is empty on success.
// InputTokens is what the API reported; a rejected call reports none.
type Call struct {
	Time        time.Time
	Purpose     string
	Task        string
	Verdict     string
	Confidence  float64
	Route       string
	Latency     time.Duration
	Error       string
	InputTokens int
}

// Client calls the Jev System One API.
type Client struct {
	Key     string
	BaseURL string
	Model   string
	HTTP    *http.Client
	// OnCall records every Screen and NeedsUser, including failures.
	// Those two may run it off the caller's goroutine.
	OnCall func(Call)
	task   string
}

// About returns a client that attributes later calls to task. The receiver
// is left unchanged, so overlapping calls do not share a task id.
func (c *Client) About(task string) *Client {
	if c == nil {
		return nil
	}
	cp := *c
	cp.task = task
	return &cp
}

// record hands the outcome to OnCall. A nil client or hook is skipped.
func (c *Client) record(purpose string, start time.Time, verdict string, confidence float64, route string, tokens int, err error) {
	if c == nil || c.OnCall == nil {
		return
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	c.OnCall(Call{
		Time: start, Purpose: purpose, Task: c.task, Verdict: verdict,
		Confidence: confidence, Route: route, Latency: time.Since(start),
		Error: msg, InputTokens: tokens,
	})
}

// FromEnv returns a client when JEV_TOKEN (or TYPESAFE_API_KEY) is set, else nil.
func FromEnv() *Client {
	key := os.Getenv("JEV_TOKEN")
	if key == "" {
		key = os.Getenv("TYPESAFE_API_KEY")
	}
	if key == "" {
		return nil
	}
	c := &Client{Key: key, BaseURL: os.Getenv("TYPESAFE_BASE_URL"), Model: os.Getenv("TYPESAFE_DEFAULT_MODEL")}
	return c
}

// Ask evaluates every question against state in one call. Rate limits and
// overload (429, 529) are retried with backoff.
func (c *Client) Ask(ctx context.Context, state string, qs map[string]Question) (map[string]Answer, error) {
	ans, _, err := c.ask(ctx, state, qs)
	return ans, err
}

// ask is Ask plus the input tokens the API reported. Tokens are 0 when the
// call fails before a usage block.
func (c *Client) ask(ctx context.Context, state string, qs map[string]Question) (map[string]Answer, int, error) {
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	model := c.Model
	if model == "" {
		model = "jev-latest"
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	body, err := json.Marshal(map[string]any{"state": state, "model": model, "questions": qs})
	if err != nil {
		return nil, 0, err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(time.Duration(250<<attempt) * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/systemone", bytes.NewReader(body))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
			last = fmt.Errorf("jev: %s", resp.Status)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, 0, fmt.Errorf("jev: %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		var out struct {
			Answers map[string]Answer `json:"answers"`
			Usage   struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, 0, fmt.Errorf("jev: decode: %w", err)
		}
		return out.Answers, out.Usage.InputTokens, nil
	}
	return nil, 0, last
}
