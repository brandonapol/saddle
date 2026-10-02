package planner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// SourceKind says where an epic comes from.
type SourceKind int

const (
	FromFile SourceKind = iota + 1
	FromStdin
	FromGitHub
)

// Source names an epic: a markdown file, stdin ("-"), or a GitHub issue
// ("gh:#12", "gh:12" or "gh:owner/repo#12"). Repo is empty for the current
// repo.
type Source struct {
	Kind  SourceKind
	Path  string
	Repo  string
	Issue int
}

var ghSource = regexp.MustCompile(`^(?:([\w.-]+/[\w.-]+))?#?(\d+)$`)

// ParseSource parses an epic source argument.
func ParseSource(arg string) (Source, error) {
	switch {
	case arg == "":
		return Source{}, errors.New("epic source is empty")
	case arg == "-":
		return Source{Kind: FromStdin}, nil
	case strings.HasPrefix(arg, "gh:"):
		m := ghSource.FindStringSubmatch(strings.TrimPrefix(arg, "gh:"))
		if m == nil {
			return Source{}, fmt.Errorf("epic source %q: want gh:#N or gh:owner/repo#N", arg)
		}
		n, _ := strconv.Atoi(m[2])
		if n <= 0 {
			return Source{}, fmt.Errorf("epic source %q: issue number must be positive", arg)
		}
		return Source{Kind: FromGitHub, Repo: m[1], Issue: n}, nil
	}
	return Source{Kind: FromFile, Path: arg}, nil
}

// Issue is a GitHub issue as an epic source sees it.
type Issue struct {
	Number int
	Title  string
	State  string
	Body   string
	URL    string
	Subs   []Issue
}

// IssueFetcher fetches an issue and its sub-issues. Repo is "owner/name", or
// empty for the current repo.
type IssueFetcher func(ctx context.Context, repo string, n int) (Issue, error)

// Epic is the text the planner plans from.
type Epic struct {
	Title string
	Text  string
	Repo  string // GitHub epics only
	Issue int    // GitHub epics only: the issue number
}

// LoadEpic reads the epic src names. stdin is read for FromStdin and fetch
// is called for FromGitHub.
func LoadEpic(ctx context.Context, src Source, stdin io.Reader, fetch IssueFetcher) (Epic, error) {
	switch src.Kind {
	case FromFile:
		b, err := os.ReadFile(src.Path)
		if err != nil {
			return Epic{}, err
		}
		name := strings.TrimSuffix(filepath.Base(src.Path), filepath.Ext(src.Path))
		return markdownEpic(string(b), name, src.Path)
	case FromStdin:
		if stdin == nil {
			return Epic{}, errors.New("epic: no stdin")
		}
		b, err := io.ReadAll(stdin)
		if err != nil {
			return Epic{}, err
		}
		return markdownEpic(string(b), "epic", "stdin")
	case FromGitHub:
		if fetch == nil {
			return Epic{}, errors.New("epic: GitHub issues are not available here")
		}
		is, err := fetch(ctx, src.Repo, src.Issue)
		if err != nil {
			return Epic{}, fmt.Errorf("epic #%d: %w", src.Issue, err)
		}
		return Epic{Title: is.Title, Text: issueText(is), Repo: src.Repo, Issue: src.Issue}, nil
	}
	return Epic{}, fmt.Errorf("epic: unknown source kind %d", src.Kind)
}

func markdownEpic(text, fallback, where string) (Epic, error) {
	if strings.TrimSpace(text) == "" {
		return Epic{}, fmt.Errorf("epic from %s is empty", where)
	}
	title := fallback
	for line := range strings.Lines(text) {
		if h, ok := strings.CutPrefix(line, "# "); ok {
			title = strings.TrimSpace(h)
			break
		}
	}
	return Epic{Title: title, Text: text}, nil
}

func issueText(is Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", is.Title)
	if is.URL != "" {
		fmt.Fprintf(&b, "%s\n\n", is.URL)
	}
	if body := strings.TrimSpace(is.Body); body != "" {
		fmt.Fprintf(&b, "%s\n", body)
	}
	if len(is.Subs) > 0 {
		b.WriteString("\n## Sub-issues\n")
		for _, s := range is.Subs {
			state := ""
			if strings.EqualFold(s.State, "closed") {
				state = " (closed)"
			}
			fmt.Fprintf(&b, "\n### #%d %s%s\n", s.Number, s.Title, state)
			if body := strings.TrimSpace(s.Body); body != "" {
				fmt.Fprintf(&b, "\n%s\n", body)
			}
		}
	}
	return b.String()
}
