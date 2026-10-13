package app

import (
	"encoding/json"
	"strings"
)

// prBodyView asks GitHub for a PR's body through gh, run in dir.
func prBodyView(dir, url string) (string, error) {
	out, err := gh(dir, "pr", "view", url, "--json", "body")
	if err != nil {
		return "", err
	}
	var pr struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return "", err
	}
	return pr.Body, nil
}

// detachedPRBody is the body of the PR restack last detached from l (one
// merged into a stack branch instead of base, see reland), as it stands on
// GitHub now, or "" when there is none or GitHub can't say. The fresh PR
// carries it untouched, so whatever the owner changed since saddle wrote it,
// such as a stripped attribution footer, stays changed (#338).
func (a *App) detachedPRBody(l landedTask) string {
	ev, ok := a.lastEvent(l.ID, "pr_merged_into_stack")
	if !ok {
		return ""
	}
	url, _, _ := strings.Cut(ev, " → ")
	if url = strings.TrimSpace(url); url == "" {
		return ""
	}
	body, err := prBodyView(a.Root, url)
	if err != nil {
		return ""
	}
	return body
}
