package app

import (
	"context"

	"github.com/brandonapol/saddle/internal/forge"
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
	ctx, f := context.Background(), a.Forge()
	is, err := f.Issue(ctx, repo, n, forge.IssueNumber|forge.IssueTitle|forge.IssueState|forge.IssueBody|forge.IssueURL|forge.IssueLabels)
	if err != nil {
		return Ticket{}, err
	}
	t := Ticket{Number: is.Number, Title: is.Title, State: is.State, Body: is.Body, URL: is.URL, Labels: is.Labels}
	// Missing sub-issue support is not an error.
	if subs, err := f.SubIssues(ctx, repo, n); err == nil {
		for _, s := range subs {
			t.SubIssues = append(t.SubIssues, SubTicket{Number: s.Number, Title: s.Title, State: s.State, Body: s.Body})
		}
	}
	return t, nil
}

// GH runs gh in the repo root and returns its trimmed stdout.
func (a *App) GH(_ context.Context, args ...string) (string, error) { return gh(a.Root, args...) }
