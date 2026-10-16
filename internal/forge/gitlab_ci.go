package forge

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// GitLab runs CI as pipelines of jobs, where GitHub has check runs. An MR's
// checks are the jobs of its head pipeline (the latest try of each, since
// the jobs API leaves retried ones out); each job's stage stands in for a
// GitHub workflow.

// jobJSON is one job of a pipeline.
type jobJSON struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Stage        string `json:"stage"`
	Status       string `json:"status"`
	AllowFailure bool   `json:"allow_failure"`
	StartedAt    string `json:"started_at"`
	WebURL       string `json:"web_url"`
	Pipeline     struct {
		ID     int    `json:"id"`
		WebURL string `json:"web_url"`
	} `json:"pipeline"`
}

// pipelineBucket sorts a pipeline's or job's status into pass, fail,
// pending, skipping or cancel, CICheck's buckets.
func pipelineBucket(status string) string {
	switch status {
	case "success":
		return "pass"
	case "failed":
		return "fail"
	case "canceled", "canceling":
		return "cancel"
	case "skipped", "manual":
		// A manual job runs only when someone starts it, so it can't hold CI.
		return "skipping"
	}
	return "pending" // created, waiting_for_resource, preparing, pending, running, scheduled
}

// bucket is j's CICheck bucket: a failure the job allows passes.
func (j jobJSON) bucket() string {
	b := pipelineBucket(j.Status)
	if b == "fail" && j.AllowFailure {
		return "pass"
	}
	return b
}

// checkRun is j as a check rollup entry in GitHub's words, which the
// watchers fold.
func (j jobJSON) checkRun() CheckRun {
	c := CheckRun{Kind: KindJob, Name: j.Name, Workflow: j.Stage, StartedAt: j.StartedAt, Status: "COMPLETED"}
	switch j.bucket() {
	case "pass":
		c.Conclusion = "SUCCESS"
		if j.Status == "failed" {
			c.Conclusion = "NEUTRAL"
		}
	case "fail":
		c.Conclusion = "FAILURE"
	case "cancel":
		c.Conclusion = "CANCELLED"
	case "skipping":
		c.Conclusion = "SKIPPED"
	default:
		c.Status = "QUEUED"
		if j.Status == "running" {
			c.Status = "IN_PROGRESS"
		}
	}
	return c
}

// jobs lists pipeline id's jobs.
func (g *GitLab) jobs(ctx context.Context, id int) ([]jobJSON, error) {
	var js []jobJSON
	err := g.apiJSON(ctx, &js, "", "projects/:id/pipelines/"+strconv.Itoa(id)+"/jobs?per_page=100")
	return js, err
}

// Checks lists the jobs of ref's head pipeline. An MR with no pipeline has
// no checks.
func (g *GitLab) Checks(ctx context.Context, ref string) ([]CICheck, error) {
	iid, err := g.iid(ctx, ref)
	if err != nil {
		return nil, err
	}
	var m mrJSON
	if err := g.apiJSON(ctx, &m, "", mrPath(iid)); err != nil {
		return nil, err
	}
	if m.HeadPipeline == nil {
		return nil, nil
	}
	js, err := g.jobs(ctx, m.HeadPipeline.ID)
	if err != nil {
		return nil, err
	}
	cs := make([]CICheck, len(js))
	for i, j := range js {
		cs[i] = CICheck{Name: j.Name, Workflow: j.Stage, Bucket: j.bucket(), Link: j.WebURL}
	}
	return cs, nil
}

var gitlabJobRe = regexp.MustCompile(`^https?://\S+?/-/jobs/(\d+)`)

// FailedLog reads a GitLab job link's trace. The run is the job's pipeline.
func (g *GitLab) FailedLog(ctx context.Context, link string, lines int) (JobLog, error) {
	m := gitlabJobRe.FindStringSubmatch(link)
	if m == nil {
		return JobLog{Run: link}, nil
	}
	l := JobLog{Run: link}
	var j jobJSON
	if err := g.apiJSON(ctx, &j, "", "projects/:id/jobs/"+m[1]); err == nil && j.Pipeline.WebURL != "" {
		l.Run = j.Pipeline.WebURL
	}
	trace, err := g.api(ctx, "", "projects/:id/jobs/"+m[1]+"/trace")
	if err != nil && trace == "" {
		return l, err
	}
	l.Step, l.Tail = cleanTrace(trace, lines)
	return l, nil
}

var (
	ansiRe    = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	sectionRe = regexp.MustCompile(`section_(start|end):\d+:([^\r\n\[]+)(?:\[[^\]]*\])?\r?`)
)

// cleanTrace turns a GitLab job trace into its last n lines of plain text
// and the section it ends in. Traces carry ANSI colors, collapsible
// section markers ("section_start:1700000000:step_script\r\x1b[0K") and
// carriage-return progress lines.
func cleanTrace(trace string, n int) (step, tail string) {
	var out []string
	for line := range strings.SplitSeq(strings.TrimRight(trace, "\n"), "\n") {
		for _, m := range sectionRe.FindAllStringSubmatch(line, -1) {
			if m[1] == "start" {
				step = m[2]
			}
		}
		line = ansiRe.ReplaceAllString(sectionRe.ReplaceAllString(line, ""), "")
		if i := strings.LastIndexByte(strings.TrimRight(line, "\r"), '\r'); i >= 0 {
			line = line[i+1:]
		}
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" && len(out) == 0 {
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return step, strings.Join(out, "\n")
}

// RerunFailed retries the failed jobs of pipeline id, or the one job of a
// "jobs/N" id (RunID of a job link whose pipeline wasn't known).
func (g *GitLab) RerunFailed(ctx context.Context, id string) error {
	path := "projects/:id/pipelines/" + id + "/retry"
	if job, ok := strings.CutPrefix(id, JobRunPrefix); ok {
		path = "projects/:id/jobs/" + job + "/retry"
	}
	_, err := g.api(ctx, "POST", path)
	return err
}
