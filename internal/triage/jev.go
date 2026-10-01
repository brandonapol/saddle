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

// Client calls the Jev System One API.
type Client struct {
	Key     string
	BaseURL string
	Model   string
	HTTP    *http.Client
}

// FromEnv returns a client when TYPESAFE_API_KEY is set, else nil.
func FromEnv() *Client {
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		return nil
	}
	c := &Client{Key: key, BaseURL: os.Getenv("TYPESAFE_BASE_URL"), Model: os.Getenv("TYPESAFE_DEFAULT_MODEL")}
	return c
}

// Ask evaluates every question against state in one call. Rate limits and
// overload (429, 529) are retried with backoff.
func (c *Client) Ask(ctx context.Context, state string, qs map[string]Question) (map[string]Answer, error) {
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
		return nil, err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(250<<attempt) * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/systemone", bytes.NewReader(body))
		if err != nil {
			return nil, err
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
			return nil, fmt.Errorf("jev: %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		var out struct {
			Answers map[string]Answer `json:"answers"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return nil, fmt.Errorf("jev: decode: %w", err)
		}
		return out.Answers, nil
	}
	return nil, last
}
