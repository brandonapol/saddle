package fakeglab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// RepoEnv names the bare repo directly, for glab calls made outside a
// checkout.
const RepoEnv = "FAKEGLAB_REPO"

// Locate finds the fake GitLab for a glab call run in dir: $FAKEGLAB_REPO,
// else the origin remote of the checkout dir is in.
func Locate(dir string) (Repo, error) {
	if d := os.Getenv(RepoEnv); d != "" {
		return Repo{Dir: d}, nil
	}
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return Repo{}, fmt.Errorf("fakeglab: no origin remote in %s: %w", dir, err)
	}
	u := strings.TrimPrefix(strings.TrimSpace(string(out)), "file://")
	if !filepath.IsAbs(u) {
		u = filepath.Join(dir, u)
	}
	return Repo{Dir: u}, nil
}

// Main runs one glab invocation in dir and returns its exit code.
func Main(dir string, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--version" {
		fmt.Fprintln(stdout, "glab 1.99.0 (fakeglab)")
		return 0
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		fmt.Fprintln(stdout, "gitlab.com\n  ✓ Logged in to gitlab.com as e2e (fakeglab)\n  ✓ Token: **************************")
		return 0
	}
	if len(args) == 0 || args[0] != "api" {
		fmt.Fprintf(stderr, "fakeglab: unsupported: glab %s\n", strings.Join(args, " "))
		return 1
	}
	r, err := Locate(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var body []byte
	err = r.Update(func(s *State) error {
		s.Calls = append(s.Calls, args)
		for prefix, msg := range s.Fail {
			if strings.HasPrefix(strings.Join(args, " "), prefix) {
				return &httpError{500, msg}
			}
		}
		c := &call{dir: r.Dir, s: s}
		v, err := c.api(args[1:])
		if err != nil {
			return err
		}
		if s, ok := v.(string); ok {
			body = []byte(s)
			return nil
		}
		body, err = json.Marshal(v)
		return err
	})
	var he *httpError
	if errors.As(err, &he) {
		// The calls log keeps failed calls too.
		_ = r.Update(func(s *State) error { s.Calls = append(s.Calls, args); return nil })
		b, _ := json.Marshal(map[string]any{"message": he.msg})
		fmt.Fprintln(stdout, string(b))
		fmt.Fprintf(stderr, "glab: %d %s (HTTP %d)\n", he.code, http.StatusText(he.code), he.code)
		return 1
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, string(body))
	return 0
}

type call struct {
	dir string // the bare repo
	s   *State
}

func notFound(what string) error { return &httpError{404, what + " Not Found"} }

// api answers `glab api [--hostname H] [-X M] endpoint [-f k=v | -F k=v]…`.
func (c *call) api(args []string) (any, error) {
	method, endpoint := "GET", ""
	fields := map[string]string{}
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-X", "--method":
			i++
			method = args[i]
		case "--hostname":
			i++
		case "-f", "-F", "--field", "--raw-field":
			i++
			k, v, _ := strings.Cut(args[i], "=")
			fields[k] = v
		case "--paginate":
		default:
			endpoint = a
		}
	}
	ep, rawq, _ := strings.Cut(strings.TrimPrefix(endpoint, "/"), "?")
	q, _ := url.ParseQuery(rawq)
	parts := strings.Split(ep, "/")
	if len(parts) < 2 || parts[0] != "projects" {
		return nil, notFound("404")
	}
	if p, _ := url.PathUnescape(parts[1]); parts[1] != ":id" && p != c.s.Path {
		return nil, notFound("404 Project")
	}
	rest := parts[2:]
	key := method + " " + strings.Join(pattern(rest), "/")
	switch key {
	case "GET ":
		return map[string]any{"id": 1, "path_with_namespace": c.s.Path, "web_url": c.s.URL(), "default_branch": c.s.Default,
			"merge_method": c.s.MergeMethod, "squash_option": c.s.SquashOption,
			"only_allow_merge_if_pipeline_succeeds": c.s.PipelineMustSucceed}, nil
	case "GET merge_requests":
		return c.listMRs(q), nil
	case "POST merge_requests":
		return c.createMR(fields)
	case "POST labels":
		if slices.Contains(c.s.Labels, fields["name"]) {
			return nil, &httpError{409, "Label already exists"}
		}
		c.s.Labels = append(c.s.Labels, fields["name"])
		return map[string]any{"name": fields["name"], "color": fields["color"]}, nil
	case "PUT labels/*":
		if name, _ := url.PathUnescape(rest[1]); !slices.Contains(c.s.Labels, name) {
			return nil, notFound("404 Label")
		}
		return map[string]any{"name": rest[1]}, nil
	case "GET issues":
		return c.listIssues(q), nil
	case "POST issues":
		c.s.NextIssue++
		is := &Issue{IID: c.s.NextIssue - 1, Title: fields["title"], Description: fields["description"], State: "opened", Labels: splitList(fields["labels"])}
		c.s.Issues = append(c.s.Issues, is)
		return c.issueJSON(is), nil
	}
	if len(rest) >= 2 && rest[0] == "merge_requests" {
		m, err := c.mr(rest[1])
		if err != nil {
			return nil, err
		}
		return c.onMR(method, m, rest[2:], fields)
	}
	if len(rest) >= 2 && rest[0] == "issues" {
		return c.onIssue(method, rest[1], rest[2:], fields)
	}
	if len(rest) == 3 && rest[0] == "pipelines" {
		return c.onPipeline(method, rest[1], rest[2])
	}
	if len(rest) == 3 && rest[0] == "jobs" && rest[2] == "retry" && method == "POST" {
		m, j := c.job(rest[1])
		if j == nil {
			return nil, notFound("404 Job")
		}
		j.Status, j.Retried = Pending, j.Retried+1
		return c.jobJSON(m, j), nil
	}
	if len(rest) >= 2 && rest[0] == "jobs" && method == "GET" {
		m, j := c.job(rest[1])
		if j == nil {
			return nil, notFound("404 Job")
		}
		if len(rest) == 3 && rest[2] == "trace" {
			return j.Trace, nil
		}
		return c.jobJSON(m, j), nil
	}
	return nil, &httpError{404, "fakeglab: unsupported " + method + " " + endpoint}
}

// pattern names a path's shape: ids become *.
func pattern(rest []string) []string {
	out := slices.Clone(rest)
	for i := 1; i < len(out); i += 2 {
		out[i] = "*"
	}
	return out
}

func splitList(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func (c *call) mr(id string) (*MR, error) {
	n, err := strconv.Atoi(id)
	if err != nil {
		return nil, notFound("404")
	}
	if m := c.s.MR(n); m != nil {
		return m, nil
	}
	return nil, notFound("404")
}

func (c *call) mrJSON(m *MR, single bool) map[string]any {
	head := m.HeadSHA
	if m.State == "opened" {
		head = branchSHA(c.dir, m.Source)
	}
	status, conflicts := mergeStatus(c.dir, c.s, m)
	ms := "can_be_merged"
	if conflicts {
		ms = "cannot_be_merged"
	}
	labels := m.Labels
	if labels == nil {
		labels = []string{}
	}
	v := map[string]any{"iid": m.IID, "id": 5000 + m.IID, "web_url": c.s.MRURL(m.IID), "title": m.Title,
		"description": m.Description, "state": m.State, "draft": strings.HasPrefix(m.Title, "Draft:"),
		"has_conflicts": conflicts, "merge_status": ms, "detailed_merge_status": status,
		"source_branch": m.Source, "target_branch": m.Target, "sha": head, "labels": labels,
		"merge_when_pipeline_succeeds": m.AutoMerge, "squash": m.Squash, "merge_commit_sha": nilIfEmpty(m.MergedSHA)}
	if single {
		var hp any
		if m.Pipeline != nil {
			hp = map[string]any{"id": m.Pipeline.ID, "status": m.Pipeline.status(), "web_url": c.pipelineURL(m.Pipeline.ID)}
		}
		v["head_pipeline"] = hp
	}
	return v
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (c *call) pipelineURL(id int) string { return fmt.Sprintf("%s/-/pipelines/%d", c.s.URL(), id) }

func (c *call) listMRs(q url.Values) []map[string]any {
	state := q.Get("state")
	limit := 20
	if n, err := strconv.Atoi(q.Get("per_page")); err == nil {
		limit = n
	}
	out := []map[string]any{}
	for i := len(c.s.MRs) - 1; i >= 0 && len(out) < limit; i-- {
		m := c.s.MRs[i]
		if state != "" && state != "all" && m.State != state {
			continue
		}
		if b := q.Get("source_branch"); b != "" && m.Source != b {
			continue
		}
		out = append(out, c.mrJSON(m, false))
	}
	return out
}

func (c *call) createMR(f map[string]string) (any, error) {
	src, tgt := f["source_branch"], f["target_branch"]
	if tgt == "" {
		tgt = c.s.Default
	}
	if branchSHA(c.dir, src) == "" {
		return nil, &httpError{422, "Source branch does not exist"}
	}
	if branchSHA(c.dir, tgt) == "" {
		return nil, &httpError{400, "target_branch is invalid"}
	}
	for _, m := range c.s.Open() {
		if m.Source == src {
			return nil, &httpError{409, fmt.Sprintf("Another open merge request already exists for this source branch: !%d", m.IID)}
		}
	}
	m := &MR{IID: c.s.NextMR, Title: f["title"], Description: f["description"], Source: src, Target: tgt, State: "opened"}
	c.s.NextMR++
	if len(c.s.DefaultJobs) > 0 {
		m.Pipeline = c.s.newPipeline(c.s.DefaultJobs)
	}
	c.s.MRs = append(c.s.MRs, m)
	return c.mrJSON(m, true), nil
}

func (c *call) systemNote(m *MR, body string) {
	c.s.Next++
	m.Notes = append(m.Notes, Note{ID: c.s.Next, Body: body, System: true})
}

func (c *call) onMR(method string, m *MR, rest []string, f map[string]string) (any, error) {
	switch method + " " + strings.Join(pattern(rest), "/") {
	case "GET ":
		return c.mrJSON(m, true), nil
	case "PUT ":
		if b, ok := f["target_branch"]; ok {
			if branchSHA(c.dir, b) == "" {
				return nil, &httpError{400, "target_branch is invalid"}
			}
			if b != m.Target {
				c.systemNote(m, fmt.Sprintf("changed target branch from `%s` to `%s`", m.Target, b))
			}
			m.Target = b
		}
		if d, ok := f["description"]; ok {
			m.Description = d
			c.systemNote(m, "changed the description")
		}
		if t, ok := f["title"]; ok {
			m.Title = t
		}
		for _, l := range splitList(f["add_labels"]) {
			if !slices.Contains(m.Labels, l) {
				m.Labels = append(m.Labels, l)
			}
			if !slices.Contains(c.s.Labels, l) {
				c.s.Labels = append(c.s.Labels, l)
			}
		}
		for _, l := range splitList(f["remove_labels"]) {
			m.Labels = slices.DeleteFunc(m.Labels, func(x string) bool { return x == l })
		}
		switch f["state_event"] {
		case "close":
			if m.State != "opened" {
				return nil, &httpError{405, "Method Not Allowed"}
			}
			m.State, m.HeadSHA = "closed", branchSHA(c.dir, m.Source)
		case "reopen":
			m.State = "opened"
		}
		return c.mrJSON(m, true), nil
	case "GET diffs":
		out := []map[string]any{}
		for _, p := range changedFiles(c.dir, m) {
			out = append(out, map[string]any{"old_path": p, "new_path": p, "diff": "@@"})
		}
		return out, nil
	case "GET notes":
		out := []map[string]any{}
		for _, n := range m.Notes {
			out = append(out, map[string]any{"id": n.ID, "body": n.Body, "system": n.System, "author": map[string]any{"username": "e2e"}})
		}
		return out, nil
	case "POST notes":
		c.s.Next++
		m.Notes = append(m.Notes, Note{ID: c.s.Next, Body: f["body"]})
		return map[string]any{"id": c.s.Next, "body": f["body"]}, nil
	case "PUT notes/*":
		id, _ := strconv.Atoi(rest[1])
		for i := range m.Notes {
			if m.Notes[i].ID == id && !m.Notes[i].System {
				m.Notes[i].Body = f["body"]
				return map[string]any{"id": id, "body": f["body"]}, nil
			}
		}
		return nil, notFound("404 Note")
	case "PUT merge":
		squash := f["squash"] == "true"
		if m.State == "opened" && m.Pipeline != nil && m.Pipeline.status() == Running && f["merge_when_pipeline_succeeds"] == "true" {
			m.AutoMerge, m.Squash = true, squash
			return c.mrJSON(m, true), nil
		}
		if err := merge(c.dir, c.s, m, squash, f["sha"]); err != nil {
			return nil, err
		}
		return c.mrJSON(m, true), nil
	}
	return nil, &httpError{404, "fakeglab: unsupported " + method + " merge_requests/" + strconv.Itoa(m.IID) + "/" + strings.Join(rest, "/")}
}

// job finds job id among the MRs' head pipelines.
func (c *call) job(id string) (*MR, *Job) {
	n, _ := strconv.Atoi(id)
	for _, m := range c.s.MRs {
		if m.Pipeline == nil {
			continue
		}
		for _, j := range m.Pipeline.Jobs {
			if j.ID == n {
				return m, j
			}
		}
	}
	return nil, nil
}

func (c *call) jobJSON(m *MR, j *Job) map[string]any {
	return map[string]any{"id": j.ID, "name": j.Name, "stage": j.Stage, "status": j.Status, "allow_failure": j.AllowFailure,
		"started_at": "2026-10-04T10:00:00Z", "web_url": fmt.Sprintf("%s/-/jobs/%d", c.s.URL(), j.ID),
		"pipeline": map[string]any{"id": m.Pipeline.ID, "web_url": c.pipelineURL(m.Pipeline.ID), "status": m.Pipeline.status()}}
}

func (c *call) onPipeline(method, id, what string) (any, error) {
	n, _ := strconv.Atoi(id)
	for _, m := range c.s.MRs {
		if m.Pipeline == nil || m.Pipeline.ID != n {
			continue
		}
		switch method + " " + what {
		case "GET jobs":
			out := []map[string]any{}
			for _, j := range m.Pipeline.Jobs {
				out = append(out, c.jobJSON(m, j))
			}
			return out, nil
		case "POST retry":
			for _, j := range m.Pipeline.Jobs {
				if j.Status == Failed || j.Status == "canceled" {
					j.Status, j.Retried = Pending, j.Retried+1
				}
			}
			return map[string]any{"id": n, "status": m.Pipeline.status(), "web_url": c.pipelineURL(n)}, nil
		}
	}
	return nil, notFound("404 Pipeline")
}

func (c *call) issueJSON(is *Issue) map[string]any {
	labels := is.Labels
	if labels == nil {
		labels = []string{}
	}
	return map[string]any{"iid": is.IID, "id": 9000 + is.IID, "project_id": 1, "title": is.Title, "description": is.Description, "state": is.State,
		"labels": labels, "web_url": fmt.Sprintf("%s/-/issues/%d", c.s.URL(), is.IID), "created_at": "2026-10-01T00:00:00Z"}
}

func (c *call) listIssues(q url.Values) []map[string]any {
	out := []map[string]any{}
	for i := len(c.s.Issues) - 1; i >= 0; i-- {
		is := c.s.Issues[i]
		if st := q.Get("state"); st != "" && st != "all" && is.State != st {
			continue
		}
		if l := q.Get("labels"); l != "" && !slices.Contains(is.Labels, l) {
			continue
		}
		out = append(out, c.issueJSON(is))
	}
	return out
}

func (c *call) onIssue(method, id string, rest []string, f map[string]string) (any, error) {
	n, _ := strconv.Atoi(id)
	var is *Issue
	for _, x := range c.s.Issues {
		if x.IID == n {
			is = x
		}
	}
	if is == nil {
		return nil, notFound("404 Issue")
	}
	switch method + " " + strings.Join(rest, "/") {
	case "GET ":
		return c.issueJSON(is), nil
	case "PUT ":
		if d, ok := f["description"]; ok {
			is.Description = d
		}
		switch f["state_event"] {
		case "close":
			is.State = "closed"
		case "reopen":
			is.State = "opened"
		}
		return c.issueJSON(is), nil
	case "POST notes":
		c.s.Next++
		is.Notes = append(is.Notes, Note{ID: c.s.Next, Body: f["body"]})
		return map[string]any{"id": c.s.Next, "body": f["body"]}, nil
	case "GET links":
		out := []map[string]any{}
		for _, x := range c.s.Issues {
			if slices.Contains(is.Links, x.IID) {
				out = append(out, c.issueJSON(x))
			}
		}
		return out, nil
	case "POST links":
		n, _ := strconv.Atoi(f["target_issue_iid"])
		if f["target_project_id"] != "1" || slices.Contains(is.Links, n) {
			return nil, &httpError{409, "Issue(s) already assigned"}
		}
		is.Links = append(is.Links, n)
		return map[string]any{"source_issue": c.issueJSON(is)}, nil
	}
	return nil, &httpError{404, "fakeglab: unsupported " + method + " issues/" + id + "/" + strings.Join(rest, "/")}
}
