package sre

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// PostmortemDir is where postmortems live, relative to the repo root.
const PostmortemDir = "docs/postmortems"

const maxSlug = 60

var nonSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// PostmortemSlug turns a title into a short lowercase filename slug.
func PostmortemSlug(title string) string {
	s := strings.Trim(nonSlugRe.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > maxSlug {
		s = strings.Trim(s[:maxSlug], "-")
	}
	if s == "" {
		return "incident"
	}
	return s
}

// PostmortemName is the file name <date>-<slug>.md, dated by the incident start.
func PostmortemName(inc PostmortemRecord) string {
	return inc.Start.UTC().Format("2006-01-02") + "-" + PostmortemSlug(inc.Title) + ".md"
}

// WritePostmortem renders inc into <root>/docs/postmortems/<date>-<slug>.md and
// returns the path. It never overwrites: if the name is taken it appends -2,
// -3, ... cfg may be nil; it only supplies check details for the runbook
// proposals. No runbook file is touched.
func WritePostmortem(root string, inc PostmortemRecord, cfg *Config) (string, error) {
	dir := filepath.Join(root, PostmortemDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	body := []byte(RenderPostmortem(inc, cfg))
	base := strings.TrimSuffix(PostmortemName(inc), ".md")
	for n := 1; ; n++ {
		name := base + ".md"
		if n > 1 {
			name = fmt.Sprintf("%s-%d.md", base, n)
		}
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(body); err != nil {
			f.Close()
			return "", err
		}
		return p, f.Close()
	}
}

// RenderPostmortem renders inc as markdown. Empty sections say so rather than
// disappearing, so the format stays predictable.
func RenderPostmortem(inc PostmortemRecord, cfg *Config) string {
	var b strings.Builder
	title := strings.TrimSpace(inc.Title)
	if title == "" {
		title = "Untitled incident"
	}
	fmt.Fprintf(&b, "# Postmortem: %s\n\n", title)
	fmt.Fprintf(&b, "- **Started:** %s\n", stamp(inc.Start))
	if inc.End.IsZero() {
		b.WriteString("- **Ended:** ongoing\n")
	} else {
		fmt.Fprintf(&b, "- **Ended:** %s\n", stamp(inc.End))
		if !inc.Start.IsZero() && inc.End.After(inc.Start) {
			fmt.Fprintf(&b, "- **Duration:** %s\n", inc.End.Sub(inc.Start).Round(time.Minute))
		}
	}

	b.WriteString("\n## Timeline\n\n")
	tl := append([]TimelineEntry(nil), inc.Timeline...)
	sort.SliceStable(tl, func(i, j int) bool { return tl[i].At.Before(tl[j].At) })
	if len(tl) == 0 {
		b.WriteString("No timeline recorded.\n")
	}
	for _, e := range tl {
		fmt.Fprintf(&b, "- %s: %s\n", stamp(e.At), e.Text)
	}

	if len(inc.Findings) > 0 {
		b.WriteString("\n## Lane findings\n\n")
		for _, f := range inc.Findings {
			fmt.Fprintf(&b, "- **%s:** %s\n", f.Lane, f.Summary)
		}
	}

	b.WriteString("\n## Root cause\n\n")
	b.WriteString(or(inc.RootCause, "Unknown: no root cause was confirmed."))
	b.WriteString("\n\n## Verification\n\n")
	if inc.VerifiedBy == "" {
		b.WriteString("Not verified by a second agent.\n")
	} else {
		fmt.Fprintf(&b, "Verified by %s.\n", inc.VerifiedBy)
	}
	if inc.Verified != "" {
		fmt.Fprintf(&b, "\n%s\n", inc.Verified)
	}

	b.WriteString("\n## Fix\n\n")
	list(&b, inc.FixPRs, "No fix recorded.")
	b.WriteString("\n## Follow-ups\n\n")
	list(&b, inc.FollowUps, "None.")

	if len(inc.FoundByChecks) > 0 {
		b.WriteString("\n## Proposed runbook additions\n\n")
		b.WriteString("Proposed only; nothing here has been added to a runbook. " +
			"Each entry is a check that found the cause, so it is worth running early next time.\n")
		for _, name := range inc.FoundByChecks {
			runbookEntry(&b, name, cfg)
		}
	}
	return b.String()
}

func runbookEntry(b *strings.Builder, name string, cfg *Config) {
	fmt.Fprintf(b, "\n### `%s`\n\n", name)
	var ch *Check
	if cfg != nil {
		for i := range cfg.Checks {
			if cfg.Checks[i].Name == name {
				ch = &cfg.Checks[i]
			}
		}
	}
	if ch == nil {
		b.WriteString("This check is not in `" + File + "`; add it, or record the manual steps that found the cause.\n")
		return
	}
	if ch.Description != "" {
		fmt.Fprintf(b, "- **When to run:** %s\n", ch.Description)
	}
	switch {
	case len(ch.Command) > 0:
		fmt.Fprintf(b, "- **Run (read-only):** `%s`\n", strings.Join(ch.Command, " "))
	case ch.Surface != "":
		fmt.Fprintf(b, "- **Look at:** `%s`\n", ch.Surface)
	}
	fmt.Fprintf(b, "- **Healthy looks like:** %s\n", ch.Expect)
	fmt.Fprintf(b, "- **Failing means:** see the root cause in this postmortem.\n")
}

func list(b *strings.Builder, items []string, empty string) {
	if len(items) == 0 {
		b.WriteString(empty + "\n")
	}
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", it)
	}
}

func or(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return strings.TrimSpace(s)
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "unknown time"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}
