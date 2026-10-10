package doctor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Groups sort results by who acts on them (#163).
const (
	GroupOK       = "ok"
	GroupFixed    = "fixed"    // saddle fixed it during this run
	GroupFixable  = "fixable"  // saddle doctor --fix (saddle init) fixes it
	GroupManual   = "manual"   // a failure only the owner can fix
	GroupOptional = "optional" // a warning worth fixing, but nothing blocks
)

// Group is who acts on r.
func (r Result) Group() string {
	switch {
	case r.Fixed:
		return GroupFixed
	case r.Status == OK:
		return GroupOK
	case r.Fixable:
		return GroupFixable
	case r.Status == Fail:
		return GroupManual
	}
	return GroupOptional
}

// local marks a result saddle init fixes.
func local(r Result) Result {
	r.Fixable = true
	return r
}

// fixInit is the fix text for every check saddle init repairs.
const fixInit = "run `saddle doctor --fix` (or saddle init)"

// about explains each check in plain words.
var about = map[string]string{
	CheckConfig:        "saddle's settings for this repo, in .saddle/config.toml",
	CheckRemote:        "the GitHub remote saddle pushes branches to and opens PRs on",
	CheckDefaultBranch: "the branch PRs land on; saddle's base must match it",
	CheckGH:            "the GitHub CLI, logged in, which saddle uses to open and merge PRs",
	CheckMergeSettings: "the repo's merge options on GitHub; stacked PRs need squash merges, not merge commits",
	CheckProtection:    "GitHub rules that keep red or unreviewed work off the base branch",
	CheckGhStack:       "GitHub's optional native Stacked PRs",
	CheckTestCmd:       "the command the merge train runs before it lands a branch",
	CheckTmux:          "runs each agent in its own hidden terminal",
	CheckClaude:        "the Claude Code CLI the agents run in",
	CheckHooks:         "git hooks that stop agents from moving saddle's own branches (saddle/*)",
	CheckPush:          "saddle itself can push branches to the remote",
	CheckOrchAllow:     "Claude permissions an orchestrator session needs so saddle's tools aren't blocked",
	CheckGate:          "the repo's own pre-commit or lint check, which saddle runs before done and landing",
	CheckRepoHooks:     "git hooks the repo ships, which agent commits should run too",
	CheckIgnored:       "keeps saddle's state and worktrees out of your commits",
	CheckStateDB:       "saddle's task database, .saddle/state.db",
	CheckLeftovers:     "old worktrees and branches saddle gc can clean up",
	CheckTrust:         "whether you said saddle may set up and run agents in this folder",
	CheckScratch:       "the one dir saddle keeps its temp files in, how full it is, and whether anything still writes to /tmp",
	CheckSkills:        "the skills and slash commands the orchestrator can use",
	"runq shims":       "wrappers that queue heavy commands (tests, builds) so agents take turns",
}

// describe fills in each result's plain-words explanation.
func describe(rs []Result) []Result {
	for i := range rs {
		if rs[i].About == "" {
			rs[i].About = about[rs[i].Name]
		}
	}
	return rs
}

// Fix runs the checks and, when any fails or warns on something saddle init
// repairs, runs init and checks again, marking what init fixed. init must
// only run in a repo the user trusts; the caller gates it. When init fails,
// Fix returns its error with the first run's results.
func Fix(env Env, init func() error) ([]Result, error) {
	before := Run(env)
	for _, r := range before {
		if r.Name == CheckOrchAllow && r.Status == Fail {
			return before, errors.New(r.Detail)
		}
	}
	broken := map[string]bool{}
	for _, r := range before {
		if r.Status != OK && r.Fixable {
			broken[r.Name] = true
		}
	}
	if len(broken) == 0 {
		return before, nil
	}
	if err := init(); err != nil {
		return before, err
	}
	if broken[CheckOrchAllow] {
		if err := fixOrchAllow(env.Root()); err != nil {
			return before, err
		}
	}
	after := Run(env)
	for i, r := range after {
		if broken[r.Name] && r.Status == OK {
			after[i].Fixed = true
		}
	}
	return after, nil
}

var slugRe = regexp.MustCompile(`github\.com[:/]([^/\s]+/[^/\s]+?)(?:\.git)?/?$`)

// repoSlug is owner/repo from a GitHub remote URL, "" for anything else.
func repoSlug(url string) string {
	m := slugRe.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return ""
	}
	return m[1]
}

// protectionSteps is the repo-specific way to protect base on GitHub.
func (r *run) protectionSteps() string {
	where := "Settings -> Branches"
	if r.repo != "" {
		where = "https://github.com/" + r.repo + "/settings/branches"
	}
	checks := "pick your CI jobs (there are no workflows in .github/workflows yet, so add CI first)"
	if jobs := workflowJobs(r.env.Root()); len(jobs) > 0 {
		checks = "pick these checks: " + strings.Join(jobs, ", ")
	}
	return fmt.Sprintf("open %s and add a rule for %s: (1) tick \"Require a pull request before merging\"; "+
		"(2) tick \"Require status checks to pass\" and %s. A check shows up in GitHub's picker only after it has run once",
		where, r.base, checks)
}

// workflowJobs lists the check names the repo's GitHub workflows report:
// each job's name, else its id. It reads only the top-level jobs: block.
func workflowJobs(root string) []string {
	var files []string
	for _, pat := range []string{"*.yml", "*.yaml"} {
		m, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", pat))
		files = append(files, m...)
	}
	sort.Strings(files)
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, j := range parseJobs(string(b)) {
			if !seen[j] {
				seen[j] = true
				out = append(out, j)
			}
		}
	}
	return out
}

func parseJobs(yml string) []string {
	var out []string
	inJobs := false
	jobIndent, childIndent := -1, -1
	var id, name string
	flush := func() {
		switch {
		case name != "" && !strings.Contains(name, "${{"):
			out = append(out, name)
		case id != "":
			out = append(out, id)
		}
		id, name, childIndent = "", "", -1
	}
	for _, line := range strings.Split(yml, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			if inJobs {
				break
			}
			inJobs = strings.HasPrefix(trimmed, "jobs:")
			continue
		}
		if !inJobs {
			continue
		}
		if jobIndent < 0 {
			jobIndent = indent
		}
		switch {
		case indent == jobIndent:
			flush()
			id = strings.Trim(strings.TrimSuffix(strings.TrimSpace(strings.SplitN(trimmed, "#", 2)[0]), ":"), `"'`)
		case indent > jobIndent && id != "":
			if childIndent < 0 {
				childIndent = indent
			}
			if v, ok := strings.CutPrefix(trimmed, "name:"); ok && indent == childIndent {
				name = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	if inJobs {
		flush()
	}
	return out
}

// sections are the grouped parts of WriteTable, in print order.
var sections = []struct{ group, title string }{
	{GroupFixed, "Fixed automatically:"},
	{GroupFixable, "Saddle can fix these: run `saddle doctor --fix` (it runs saddle init; your config.toml is kept):"},
	{GroupManual, "You need to do this:"},
	{GroupOptional, "Optional:"},
}

// writeGroups prints every result that isn't plain ok under its group, then
// the next steps once nothing blocks.
func writeGroups(w io.Writer, rs []Result) {
	for _, s := range sections {
		var in []Result
		for _, r := range rs {
			if r.Group() == s.group {
				in = append(in, r)
			}
		}
		if len(in) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", s.title)
		for _, r := range in {
			head := r.Name
			if r.About != "" {
				head += " (" + r.About + ")"
			}
			fmt.Fprintf(w, "  - %s\n      %s\n", head, r.Detail)
			if r.Fix != "" && s.group != GroupFixed {
				fmt.Fprintf(w, "      fix: %s\n", strings.ReplaceAll(r.Fix, "\n", "\n      "))
			}
		}
	}
	if !Failed(rs) {
		fmt.Fprint(w, "\nNext steps:\n"+
			"  saddle up           open saddle and tell the orchestrator what to work on\n"+
			"  saddle up epic.md   or hand it an epic: it plans it and shows you the plan first\n")
	}
}
