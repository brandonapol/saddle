package triage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func fakeJev(t *testing.T, answers map[string]Answer, status ...int) *Client {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("bad request %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body struct {
			State     string              `json:"state"`
			Model     string              `json:"model"`
			Questions map[string]Question `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "jev-latest" || len(body.Questions) == 0 {
			t.Errorf("bad body %+v %v", body, err)
		}
		if calls < len(status) {
			w.WriteHeader(status[calls])
			calls++
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.13.0", "answers": answers})
	}))
	t.Cleanup(srv.Close)
	return &Client{Key: "k", BaseURL: srv.URL}
}

const permScreen = " Do you want to create hello.txt?\n ❯ 1. Yes\n   2. Yes, and don't ask again\n   3. No"

func TestScreenRoutes(t *testing.T) {
	cases := []struct {
		name   string
		answer Answer
		screen string
		want   string
	}{
		{"working drops", Answer{Choice: "working", Confidence: 0.9}, "✻ Cooking…", Drop},
		{"unsure working still escalates", Answer{Choice: "working", Confidence: 0.4}, "✻ Cooking…", Orchestrator},
		{"routine prompt auto-approves", Answer{Choice: "routine_prompt", Confidence: 0.95}, permScreen, AutoApprove},
		{"routine but unsure", Answer{Choice: "routine_prompt", Confidence: 0.7}, permScreen, Orchestrator},
		{"routine but no Yes option", Answer{Choice: "routine_prompt", Confidence: 0.99}, "❯ No, exit\n  Yes, allow", Orchestrator},
		{"risky goes to human", Answer{Choice: "risky_prompt", Confidence: 0.8}, permScreen, Human},
		{"question goes to human", Answer{Choice: "question", Confidence: 0.9}, "Which approach do you prefer?", Human},
		{"stuck goes to orchestrator", Answer{Choice: "stuck", Confidence: 0.9}, "Error: rate limited", Orchestrator},
	}
	for _, c := range cases {
		cl := fakeJev(t, map[string]Answer{"verdict": c.answer})
		d, err := Screen(context.Background(), cl, "create hello.txt", c.screen)
		if err != nil {
			t.Fatal(err)
		}
		if d.Route != c.want {
			t.Errorf("%s: route = %s, want %s", c.name, d.Route, c.want)
		}
		if d.Route == AutoApprove && (len(d.Keys) != 1 || d.Keys[0] != "1") {
			t.Errorf("%s: keys = %v", c.name, d.Keys)
		}
	}
}

func TestRetriesOnRateLimit(t *testing.T) {
	cl := fakeJev(t, map[string]Answer{"needs_user": {Type: "noul", Noul: 0.92}}, http.StatusTooManyRequests, 529)
	ok, err := NeedsUser(context.Background(), cl, "t3 wants to delete build/. Approve?")
	if err != nil || !ok {
		t.Fatalf("NeedsUser = %v, %v", ok, err)
	}
}

func TestErrorFallsBackToOrchestrator(t *testing.T) {
	cl := fakeJev(t, nil, http.StatusUnauthorized)
	d, err := Screen(context.Background(), cl, "x", permScreen)
	if err == nil || d.Route != Orchestrator {
		t.Fatalf("got %+v, %v", d, err)
	}
}
