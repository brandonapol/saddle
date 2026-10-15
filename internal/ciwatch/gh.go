package ciwatch

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/brandonapol/saddle/internal/forge"
)

// ExecRunner runs the real gh CLI in dir.
func ExecRunner(dir string) Runner {
	return func(ctx context.Context, args ...string) (string, error) {
		out, errb, err := forge.Output(ctx, dir, forge.GH, args...)
		if err != nil {
			n := min(len(args), 2)
			return out, fmt.Errorf("gh %s: %w: %s", strings.Join(args[:n], " "), err, strings.TrimSpace(errb))
		}
		return out, nil
	}
}

// prChecks asks gh for the checks on ref, a PR number, URL or branch.
func prChecks(ctx context.Context, gh Runner, ref string) ([]Check, error) {
	out, err := gh(ctx, "pr", "checks", ref, "--json", "name,workflow,bucket,link")
	// gh exits non-zero for pending or failing checks but still prints the
	// JSON, so trust the output whenever it parses.
	if s := strings.TrimSpace(out); strings.HasPrefix(s, "[") {
		var cs []Check
		if jerr := json.Unmarshal([]byte(s), &cs); jerr == nil {
			return cs, nil
		} else if err == nil {
			return nil, fmt.Errorf("gh pr checks %s: %w", ref, jerr)
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "no checks reported") {
			return nil, nil
		}
		return nil, err
	}
	return nil, nil
}

var jobLinkRe = regexp.MustCompile(`^(https://[^/]+/[^/]+/[^/]+/actions/runs/\d+)(?:/attempts/\d+)?/job/(\d+)`)

// parseJobLink splits a GitHub Actions job link into the run URL and job id.
func parseJobLink(link string) (run, job string, ok bool) {
	m := jobLinkRe.FindStringSubmatch(link)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// cleanLog turns `gh run view --log-failed` output, whose lines are
// "job<TAB>step<TAB>timestamp text", into its last n lines of plain text and
// the step the log ends in.
func cleanLog(log string, n int) (step, tail string) {
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		step = parts[1]
		text := parts[2]
		// Drop the RFC 3339 timestamp gh puts before each line.
		if sp := strings.IndexByte(text, ' '); sp > 0 && strings.HasSuffix(text[:sp], "Z") && strings.Contains(text[:sp], "T") {
			text = text[sp+1:]
		}
		lines[i] = text
	}
	return step, strings.Join(lines, "\n")
}
