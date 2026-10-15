package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Checks asks `gh pr checks`. gh exits non-zero for pending or failing
// checks but still prints the JSON, so the output is trusted whenever it
// parses.
func (g *GitHub) Checks(ctx context.Context, ref string) ([]CICheck, error) {
	out, err := g.Run(ctx, "pr", "checks", ref, "--json", "name,workflow,bucket,link")
	if s := strings.TrimSpace(out); strings.HasPrefix(s, "[") {
		var cs []CICheck
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

// FailedLog reads `gh run view --job J --log-failed` for a GitHub Actions
// job link. A log that came back is used even when gh then failed.
func (g *GitHub) FailedLog(ctx context.Context, link string, lines int) (JobLog, error) {
	run, job, ok := parseJobLink(link)
	if !ok {
		return JobLog{Run: link}, nil
	}
	l := JobLog{Run: run}
	log, err := g.Run(ctx, "run", "view", "--job", job, "--log-failed")
	if err != nil && log == "" {
		return l, err
	}
	l.Step, l.Tail = cleanLog(log, lines)
	return l, nil
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
