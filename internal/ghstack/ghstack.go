// Package ghstack drives GitHub's native stacked PRs through the gh-stack
// extension (github/gh-stack), over an injected gh runner (#211).
//
// Saddle owns its branches and only its merge train pushes them, so this
// package uses just the commands that don't track stacks locally or push:
// `gh stack link` with PR URLs (bottom to top), `gh stack merge <n> --yes`,
// and the read-only stacks REST endpoint to view stacks and to probe whether
// the repo has Stacked PRs. It never runs init, add, submit, sync or push.
package ghstack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Runner runs gh with args and returns its stdout; a failure's error carries
// gh's stderr.
type Runner func(args ...string) (string, error)

// Error kinds. Test with errors.Is.
var (
	// ErrNotInstalled: the gh-stack extension isn't installed.
	ErrNotInstalled = errors.New("the gh-stack extension isn't installed (gh extension install github/gh-stack)")
	// ErrNotEnabled: the repo doesn't have Stacked PRs (a private preview, enabled per repo).
	ErrNotEnabled = errors.New("stacked PRs aren't enabled for this repository")
	// ErrConflict: a PR already belongs to another stack on GitHub.
	ErrConflict = errors.New("a PR already belongs to another stack on GitHub")
	// ErrNoStack: no stack on GitHub holds the ref.
	ErrNoStack = errors.New("no stack on GitHub holds it")
	// ErrMergeRefused: GitHub refused the atomic stack merge; nothing merged.
	ErrMergeRefused = errors.New("GitHub refused the stack merge")
)

// Error is a failed gh-stack operation: Kind is one of the Err values above
// (nil when gh failed some other way) and Err is what gh said.
type Error struct {
	Op   string // version, probe, link, view or merge
	Kind error
	Err  error
}

func (e *Error) Error() string {
	if e.Kind == nil {
		return fmt.Sprintf("gh stack %s: %v", e.Op, e.Err)
	}
	return fmt.Sprintf("gh stack %s: %v: %v", e.Op, e.Kind, e.Err)
}

// Unwrap lets errors.Is match the kind and the underlying error.
func (e *Error) Unwrap() []error {
	if e.Kind == nil {
		return []error{e.Err}
	}
	return []error{e.Kind, e.Err}
}

// classify types what gh printed. merge marks a merge, where an unexplained
// failure is GitHub refusing it.
func classify(op string, err error) error {
	msg := strings.ToLower(err.Error())
	var kind error
	switch {
	case strings.Contains(msg, `unknown command "stack"`):
		kind = ErrNotInstalled
	case strings.Contains(msg, "not enabled"), strings.Contains(msg, "is not available for this repository"),
		op == "probe" && strings.Contains(msg, "http 404"):
		kind = ErrNotEnabled
	case strings.Contains(msg, "belongs to multiple stacks"), strings.Contains(msg, "belongs to a different stack"),
		strings.Contains(msg, "already contains"):
		kind = ErrConflict
	case op == "merge":
		kind = ErrMergeRefused
	}
	return &Error{Op: op, Kind: kind, Err: err}
}

// Client is gh-stack in one repo.
type Client struct{ Run Runner }

// Available returns the installed extension's version, or ErrNotInstalled.
func (c Client) Available() (string, error) {
	out, err := c.Run("stack", "--version")
	if err != nil {
		e := classify("version", err)
		if !errors.Is(e, ErrNotInstalled) {
			e = &Error{Op: "version", Kind: ErrNotInstalled, Err: err}
		}
		return "", e
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return "", &Error{Op: "version", Kind: ErrNotInstalled, Err: errors.New("gh stack --version printed nothing")}
	}
	return strings.TrimPrefix(f[len(f)-1], "v"), nil
}

// stacksPath is the repo's stacks REST endpoint; gh fills in owner and repo.
const stacksPath = "repos/{owner}/{repo}/stacks"

// Enabled probes, read-only, whether the repo has Stacked PRs: it lists the
// repo's stacks. A refusal is (false, ErrNotEnabled); anything else that
// fails is (false, err): unknown counts as disabled.
func (c Client) Enabled() (bool, error) {
	if _, err := c.Run("api", stacksPath+"?per_page=1"); err != nil {
		return false, classify("probe", err)
	}
	return true, nil
}

// Link creates or updates a stack on GitHub from refs, bottom to top, its
// bottom targeting base. Pass PR URLs or numbers: a branch makes gh-stack
// push it, and only saddle's train pushes saddle's branches.
func (c Client) Link(base string, refs ...string) error {
	if len(refs) < 2 {
		return &Error{Op: "link", Err: fmt.Errorf("a stack needs at least two PRs, got %d", len(refs))}
	}
	args := []string{"stack", "link"}
	if base != "" {
		args = append(args, "--base", base)
	}
	if _, err := c.Run(append(args, refs...)...); err != nil {
		return classify("link", err)
	}
	return nil
}

// Stack is a stack on GitHub.
type Stack struct {
	Number int    `json:"number"`
	Base   string `json:"base"`
	Open   bool   `json:"open"`
	PRs    []PR   `json:"prs"` // bottom to top
}

// PR is one pull request in a stack.
type PR struct {
	Number   int    `json:"number"`
	State    string `json:"state"` // open or closed
	Draft    bool   `json:"draft,omitempty"`
	MergedAt string `json:"merged_at,omitempty"`
	Head     string `json:"head"`
}

// Merged reports whether the PR merged.
func (p PR) Merged() bool { return p.MergedAt != "" }

// Top is the stack's top PR number, 0 for an empty stack.
func (s Stack) Top() int {
	if len(s.PRs) == 0 {
		return 0
	}
	return s.PRs[len(s.PRs)-1].Number
}

type apiStack struct {
	Number int `json:"number"`
	Base   struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Open bool `json:"open"`
	PRs  []struct {
		Number   int     `json:"number"`
		State    string  `json:"state"`
		Draft    bool    `json:"draft"`
		MergedAt *string `json:"merged_at"`
		Head     struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_requests"`
}

// Stacks lists the repo's stacks on GitHub.
func (c Client) Stacks() ([]Stack, error) {
	out, err := c.Run("api", stacksPath+"?per_page=100", "--paginate")
	if err != nil {
		return nil, classify("view", err)
	}
	var stacks []Stack
	dec := json.NewDecoder(bytes.NewBufferString(out))
	for {
		var page []apiStack
		if err := dec.Decode(&page); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, &Error{Op: "view", Err: fmt.Errorf("reading stacks: %w", err)}
		}
		for _, a := range page {
			s := Stack{Number: a.Number, Base: a.Base.Ref, Open: a.Open}
			for _, p := range a.PRs {
				pr := PR{Number: p.Number, State: p.State, Draft: p.Draft, Head: p.Head.Ref}
				if p.MergedAt != nil {
					pr.MergedAt = *p.MergedAt
				}
				s.PRs = append(s.PRs, pr)
			}
			stacks = append(stacks, s)
		}
	}
	return stacks, nil
}

// PRNumber reads a PR number from n, #n or a PR URL; 0 if ref is none.
func PRNumber(ref string) int {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "#")
	if i := strings.LastIndex(ref, "/pull/"); i >= 0 {
		ref = ref[i+len("/pull/"):]
	}
	n, err := strconv.Atoi(ref)
	if err != nil {
		return 0
	}
	return n
}

// View finds the stack holding ref: a PR number, #number or URL, a head
// branch, or a stack number. An open stack wins over a closed one.
func (c Client) View(ref string) (Stack, error) {
	stacks, err := c.Stacks()
	if err != nil {
		return Stack{}, err
	}
	n := PRNumber(ref)
	var found []Stack
	for _, s := range stacks {
		for _, p := range s.PRs {
			if (n > 0 && p.Number == n) || p.Head == ref {
				found = append(found, s)
				break
			}
		}
	}
	if len(found) == 0 && n > 0 && !strings.Contains(ref, "/pull/") {
		for _, s := range stacks {
			if s.Number == n {
				found = append(found, s)
			}
		}
	}
	for _, s := range found {
		if s.Open {
			return s, nil
		}
	}
	if len(found) > 0 {
		return found[0], nil
	}
	return Stack{}, &Error{Op: "view", Kind: ErrNoStack, Err: fmt.Errorf("%s", ref)}
}

// Merge merges, atomically, the stack numbered ref or everything in its
// stack up to and including PR ref, without prompting. An empty method uses
// the one last used.
func (c Client) Merge(ref, method string) error {
	args := []string{"stack", "merge", ref, "--yes"}
	if method != "" {
		args = append(args, "--merge-method", method)
	}
	if _, err := c.Run(args...); err != nil {
		return classify("merge", err)
	}
	return nil
}
