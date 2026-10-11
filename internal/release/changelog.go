package release

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// PR is a merged pull request as gh pr list --json number,title,labels
// reports it, with the labels flattened to names.
type PR struct {
	Number int
	Title  string
	Labels []string
}

// Section is one heading of a release's notes.
type Section struct {
	Title string
	PRs   []PR
}

// Header opens CHANGELOG.md.
const Header = "# Changelog\n\nAll notable changes to saddle, newest first. Each section is generated from the\npull requests merged since the previous tag by `saddle changelog`; see\ndocs/RELEASING.md.\n"

// sectionOrder is the order sections appear in; empty ones are left out.
var sectionOrder = []string{"Breaking changes", "Features", "Fixes", "Performance", "Documentation", "Build and CI", "Other changes"}

var labelSection = map[string]string{
	"breaking": "Breaking changes", "breaking-change": "Breaking changes",
	"enhancement": "Features", "feature": "Features",
	"bug": "Fixes", "fix": "Fixes",
	"performance": "Performance", "perf": "Performance",
	"documentation": "Documentation", "docs": "Documentation",
	"ci": "Build and CI", "build": "Build and CI", "dependencies": "Build and CI",
}

var typeSection = map[string]string{
	"feat": "Features", "fix": "Fixes", "perf": "Performance", "docs": "Documentation",
	"ci": "Build and CI", "build": "Build and CI",
}

// prefixRE matches a conventional prefix ("fix(hook)!: ") or a package
// prefix ("store: ") at the start of a title.
var prefixRE = regexp.MustCompile(`^([a-z][a-z0-9/_.-]*)(\([^)]*\))?(!)?:\s+`)

// attributionRE matches the AI co-author and attribution lines release
// notes must never carry.
var (
	attributionLineRE = regexp.MustCompile(`(?i)^\s*(co-authored-by:|claude-session:|🤖|generated with \[?claude)`)
	attributionTailRE = regexp.MustCompile(`(?i)\s*(🤖|generated with \[?claude code|co-authored-by:).*$`)
)

// StripAttribution drops co-author trailers and "Generated with" lines.
func StripAttribution(text string) string {
	var keep []string
	for line := range strings.SplitSeq(text, "\n") {
		if !attributionLineRE.MatchString(line) {
			keep = append(keep, line)
		}
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// classify picks a PR's section: a label wins, then a conventional prefix
// ("!" before the colon is breaking), else Other changes.
func classify(p PR) string {
	if slices.ContainsFunc(p.Labels, func(l string) bool { return labelSection[strings.ToLower(l)] == "Breaking changes" }) {
		return "Breaking changes"
	}
	m := prefixRE.FindStringSubmatch(p.Title)
	if m != nil && m[3] == "!" {
		return "Breaking changes"
	}
	for _, l := range p.Labels {
		if s, ok := labelSection[strings.ToLower(l)]; ok {
			return s
		}
	}
	if m != nil {
		if s, ok := typeSection[m[1]]; ok {
			return s
		}
	}
	return "Other changes"
}

// cleanTitle drops the prefix (the section already says it) and any
// attribution pasted into the title.
func cleanTitle(t string) string {
	t = attributionTailRE.ReplaceAllString(t, "")
	return strings.TrimSpace(prefixRE.ReplaceAllString(strings.TrimSpace(t), ""))
}

// Group sorts prs into sections, in sectionOrder, keeping each section's PRs
// in number order.
func Group(prs []PR) []Section {
	by := map[string][]PR{}
	for _, p := range prs {
		s := classify(p)
		by[s] = append(by[s], p)
	}
	var out []Section
	for _, title := range sectionOrder {
		ps := by[title]
		if len(ps) == 0 {
			continue
		}
		slices.SortFunc(ps, func(a, b PR) int { return a.Number - b.Number })
		out = append(out, Section{Title: title, PRs: ps})
	}
	return out
}

// Render writes one CHANGELOG.md section for version, released on date.
func Render(version, date string, prs []PR) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s (%s)\n", version, date)
	groups := Group(prs)
	if len(groups) == 0 {
		b.WriteString("\nNo merged pull requests.\n")
	}
	for _, s := range groups {
		fmt.Fprintf(&b, "\n### %s\n\n", s.Title)
		for _, p := range s.PRs {
			fmt.Fprintf(&b, "- %s (#%d)\n", cleanTitle(p.Title), p.Number)
		}
	}
	return b.String()
}

// Insert adds section (from Render) to changelog as its newest entry. It
// refuses a version that already has a section.
func Insert(changelog, section string) (string, error) {
	heading, _, _ := strings.Cut(section, "\n")
	version, _, _ := strings.Cut(strings.TrimPrefix(heading, "## "), " ")
	if _, ok := Extract(changelog, version); ok {
		return "", fmt.Errorf("CHANGELOG.md already has a section for %s", version)
	}
	if strings.TrimSpace(changelog) == "" {
		changelog = Header
	}
	section = strings.TrimRight(section, "\n") + "\n"
	if i := strings.Index(changelog, "\n## "); i >= 0 {
		return changelog[:i+1] + section + "\n" + changelog[i+1:], nil
	}
	return strings.TrimRight(changelog, "\n") + "\n\n" + section, nil
}

// Extract returns the body of version's section, without its heading: the
// GitHub Release notes.
func Extract(changelog, version string) (string, bool) {
	lines := strings.Split(changelog, "\n")
	for i, l := range lines {
		if l != "## "+version && !strings.HasPrefix(l, "## "+version+" ") {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "## ") {
				end = j
				break
			}
		}
		return strings.TrimSpace(strings.Join(lines[i+1:end], "\n")) + "\n", true
	}
	return "", false
}
