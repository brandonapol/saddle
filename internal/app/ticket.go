package app

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// Ticket is a GitHub issue the orchestrator can turn into tasks.
type Ticket struct {
	Number    int         `json:"number"`
	Title     string      `json:"title"`
	State     string      `json:"state"`
	Body      string      `json:"body"`
	Labels    []string    `json:"labels,omitempty"`
	URL       string      `json:"url"`
	SubIssues []SubTicket `json:"sub_issues,omitempty"`
}

type SubTicket struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Body   string `json:"body,omitempty"`
}

// Ticket fetches an issue and its sub-issues with gh.
func (a *App) Ticket(n int) (Ticket, error) { return a.TicketIn("", n) }

// TicketIn fetches an issue and its sub-issues from repo ("owner/name"), or
// from the current repo when repo is empty.
func (a *App) TicketIn(repo string, n int) (Ticket, error) {
	var raw struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Body   string `json:"body"`
		URL    string `json:"url"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	args := []string{"issue", "view", strconv.Itoa(n), "--json", "number,title,state,body,url,labels"}
	api := "repos/{owner}/{repo}"
	if repo != "" {
		args = append(args[:3], append([]string{"-R", repo}, args[3:]...)...)
		api = "repos/" + repo
	}
	out, err := gh(a.Root, args...)
	if err != nil {
		return Ticket{}, err
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return Ticket{}, fmt.Errorf("parse issue #%d: %w", n, err)
	}
	t := Ticket{Number: raw.Number, Title: raw.Title, State: raw.State, Body: raw.Body, URL: raw.URL}
	for _, l := range raw.Labels {
		t.Labels = append(t.Labels, l.Name)
	}
	// Sub-issues aren't in `gh issue view`; ask the REST API. Missing support is not an error.
	if out, err := gh(a.Root, "api", api+"/issues/"+strconv.Itoa(n)+"/sub_issues",
		"--jq", "[.[] | {number, title, state, body}]"); err == nil && out != "" {
		_ = json.Unmarshal([]byte(out), &t.SubIssues)
	}
	return t, nil
}
