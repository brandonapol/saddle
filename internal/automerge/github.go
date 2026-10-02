package automerge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// Runner runs gh with args and returns its stdout.
type Runner func(args ...string) (string, error)

// ExecRunner runs the gh CLI in dir.
func ExecRunner(dir string) Runner {
	return func(args ...string) (string, error) {
		cmd := exec.Command("gh", args...)
		cmd.Dir = dir
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("gh %s: %w: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(errb.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}
}

// GH is GitHub through the gh CLI.
type GH struct {
	Run    Runner
	method string // cached once read
}

// prFields are the fields PR reads.
const prFields = "url,state,isDraft,mergeable,mergeStateStatus,baseRefName,headRefName,headRefOid,labels,statusCheckRollup"

func (g *GH) PR(url string) (PR, error) {
	out, err := g.Run("pr", "view", url, "--json", prFields)
	if err != nil {
		return PR{}, err
	}
	var v struct {
		PR
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Rollup []Check `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return PR{}, fmt.Errorf("gh pr view %s: %w", url, err)
	}
	pr := v.PR
	pr.Labels = nil
	for _, l := range v.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	pr.Checks = summarize(v.Rollup)
	if pr.URL == "" {
		pr.URL = url
	}
	return pr, nil
}

// MergeMethod is squash when the repo allows it, else rebase. Saddle needs
// linear history, so it never picks merge commits.
func (g *GH) MergeMethod() (string, error) {
	if g.method != "" {
		return g.method, nil
	}
	out, err := g.Run("api", "repos/{owner}/{repo}")
	if err != nil {
		return "", err
	}
	var r struct {
		Squash *bool `json:"allow_squash_merge"`
		Rebase *bool `json:"allow_rebase_merge"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return "", fmt.Errorf("parse gh api output: %w", err)
	}
	switch {
	case r.Squash == nil || r.Rebase == nil:
		return "", errors.New("gh api did not return the repo's merge settings (they need push access)")
	case *r.Squash:
		g.method = "squash"
	case *r.Rebase:
		g.method = "rebase"
	default:
		return "", errors.New("the repo allows neither squash nor rebase merges, and saddle needs linear history")
	}
	return g.method, nil
}

// Merge merges url with method if its head is still head. It never passes
// --admin, so branch protection always applies.
func (g *GH) Merge(url, method, head string) error {
	if !slices.Contains([]string{"squash", "rebase", "merge"}, method) {
		return fmt.Errorf("unknown merge method %q", method)
	}
	args := []string{"pr", "merge", url, "--" + method}
	if head != "" {
		args = append(args, "--match-head-commit", head)
	}
	_, err := g.Run(args...)
	return err
}
