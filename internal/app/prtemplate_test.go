package app

import (
	"regexp"
	"strings"
	"testing"
)

// quarkTemplate is autobutler-org/quark's PR template, the one #361 was
// found against.
const quarkTemplate = `Closes #N

## What

One paragraph. What changed and why.

## Changes

- Bullet list of specific changes

## PR Type

- [ ] Bug fix
- [ ] Feature
- [ ] Refactor
- [ ] Docs / content
- [ ] Chore / tooling
- [ ] Tests

## Surface

- [ ] Backend (Go)
- [ ] Frontend (Flutter)
- [ ] API / swagger
- [ ] CI / workflows
- [ ] Docs / content only

## Testing

- [ ] Local testing recommended (UI changes, routing, behavior changes)
- [ ] Local testing not needed (logic-only, docs, trivial change)
- [ ] Includes new automated tests
- [ ] Manually tested by author

## Bot Review Guidance

<!-- Optional: direct Sable / ExoKomodo attention to the gnarly part,
     e.g. "focus on concurrency in pkg/util/eventbus" or "verify no broken image refs" -->

## Notes

<!-- Optional. Tradeoffs, follow-ups, things reviewers should know. -->
`

var checkboxRe = regexp.MustCompile(`(?m)^\s*[-*] \[[ xX]\] (.*)$`)

// checkQuarkBody fails t unless body follows quarkTemplate strictly: every
// heading in order, every checkbox, a ticked box under PR Type and Surface,
// placeholders and prompts gone.
func checkQuarkBody(t *testing.T, body string, issue string) {
	t.Helper()
	body += "\n"
	at := 0
	for _, h := range []string{"## What", "## Changes", "## PR Type", "## Surface", "## Testing", "## Bot Review Guidance", "## Notes"} {
		i := strings.Index(body[at:], "\n"+h+"\n")
		if i < 0 {
			t.Fatalf("heading %q missing or out of order:\n%s", h, body)
		}
		at += i + 1
	}
	for _, m := range checkboxRe.FindAllStringSubmatch(quarkTemplate, -1) {
		if !strings.Contains(body, "] "+m[1]+"\n") {
			t.Fatalf("checkbox %q missing:\n%s", m[1], body)
		}
	}
	for _, sec := range []string{"## PR Type", "## Surface"} {
		part := body[strings.Index(body, sec):]
		part = part[len(sec):]
		if j := strings.Index(part, "\n## "); j >= 0 {
			part = part[:j]
		}
		if !strings.Contains(part, "- [x] ") {
			t.Fatalf("nothing ticked under %s:\n%s", sec, body)
		}
	}
	for _, lit := range []string{"#N\n", "<!--", "Bullet list of specific changes", "One paragraph. What changed and why."} {
		if strings.Contains(body, lit) {
			t.Fatalf("template placeholder %q left literal:\n%s", lit, body)
		}
	}
	if issue != "" && strings.Count(body, "Closes "+issue) != 1 {
		t.Fatalf("body should close %s once:\n%s", issue, body)
	}
}

func TestFillPRTemplateQuark(t *testing.T) {
	t.Parallel()
	body := fillPRTemplate(quarkTemplate, prText{
		Title:   "fix: login crash on empty token",
		Desc:    "Stops the login handler crashing on an empty token.",
		Issues:  []int{3128},
		Commits: []string{"fix: login crash on empty token", "test: cover empty token"},
		Files:   []string{"pkg/auth/login.go", "pkg/auth/login_test.go"},
	})
	checkQuarkBody(t, body, "#3128")
	for _, want := range []string{
		"## What\n\nStops the login handler crashing on an empty token.\n",
		"## Changes\n\n- fix: login crash on empty token\n- test: cover empty token\n",
		"- [x] Bug fix\n", "- [ ] Feature\n",
		"- [x] Backend (Go)\n", "- [ ] Frontend (Flutter)\n", "- [ ] Docs / content only\n",
		"- [x] Includes new automated tests\n", "- [ ] Manually tested by author\n",
		"## Bot Review Guidance\n\n## Notes",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %q:\n%s", want, body)
		}
	}
	if !strings.HasPrefix(body, "Closes #3128\n") {
		t.Fatalf("Closes line not filled in place:\n%s", body)
	}
}

func TestFillPRTemplateTicksDocsOnly(t *testing.T) {
	t.Parallel()
	body := fillPRTemplate(quarkTemplate, prText{
		Title: "Describe the sync flow", Desc: "Docs.",
		Files: []string{"docs/sync.md", "README.md"},
	})
	for _, want := range []string{"- [x] Docs / content\n", "- [x] Docs / content only\n", "- [ ] Backend (Go)\n", "- [ ] Feature\n"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body lacks %q:\n%s", want, body)
		}
	}
	// No issue to close: the placeholder line goes rather than stay literal.
	if strings.Contains(body, "Closes") {
		t.Fatalf("Closes placeholder kept with no issue:\n%s", body)
	}
}

func TestFillPRTemplateNoPlaceholderClosesUnderDescription(t *testing.T) {
	t.Parallel()
	tmpl := "## Summary\n<!-- what and why -->\n\n## Testing\n- [ ] make check\n"
	body := fillPRTemplate(tmpl, prText{Desc: "Adds two.", Issues: []int{42}})
	want := "## Summary\n\nAdds two.\n\nCloses #42\n\n## Testing\n\n- [ ] make check"
	if body != want {
		t.Fatalf("body =\n%s\nwant\n%s", body, want)
	}
}

func TestFillPRTemplateKeepsAttributionLast(t *testing.T) {
	t.Parallel()
	footer := "🤖 Generated with [Claude Code](https://claude.com/claude-code)"
	desc := "Adds two.\n\n" + footer
	for name, tmpl := range map[string]string{
		"none":      "",
		"headless":  "Thanks for the PR!\n",
		"headings":  quarkTemplate,
		"no prompt": "## Summary\n\n## Testing\n- [ ] make check\n",
	} {
		body := fillPRTemplate(tmpl, prText{Desc: desc, Issues: []int{7}, Note: "Published by `saddle publish`."})
		if !strings.HasSuffix(strings.TrimSpace(body), footer) || strings.Count(body, footer) != 1 {
			t.Fatalf("%s: attribution not last:\n%s", name, body)
		}
		if !strings.Contains(body, "Adds two.") || !strings.Contains(body, "Closes #7") || !strings.Contains(body, "Published by") {
			t.Fatalf("%s: body lost content:\n%s", name, body)
		}
	}
}

// A repo with no template, or one with no headings, gets the body it got
// before #361.
func TestFillPRTemplateFallsBackAsBefore(t *testing.T) {
	t.Parallel()
	if got := fillPRTemplate("", prText{Desc: "Adds two.", Issues: []int{42, 43}}); got != "Adds two.\n\nCloses #42\n\nCloses #43" {
		t.Fatalf("no template: %q", got)
	}
	if got := fillPRTemplate("  \n", prText{Desc: "Adds two."}); got != "Adds two." {
		t.Fatalf("blank template: %q", got)
	}
	tmpl := "Please describe the change.\n- [ ] I ran make check\n"
	if got := fillPRTemplate(tmpl, prText{Desc: "Adds two.", Issues: []int{42}}); got != "Adds two.\n\nCloses #42\n\n"+strings.TrimSpace(tmpl) {
		t.Fatalf("headingless template: %q", got)
	}
}

// onlyCall cuts the gh calls logged after call off it.
func onlyCall(call string) string {
	for _, next := range []string{"\npr ", "\nissue ", "\napi "} {
		if i := strings.Index(call, next); i >= 0 {
			call = call[:i]
		}
	}
	return call
}

// prsSetup is a repo whose main holds tmpl as its PR template (if any), with
// an origin and a fake gh.
func prsSetup(t *testing.T, tmpl string) (*App, func() []string) {
	t.Helper()
	a := trainSetup(t)
	if tmpl != "" {
		write(t, a.Root, ".github/pull_request_template.md", tmpl)
		commitAll(t, a.Root, "template")
	}
	must(t, a.Init())
	_, ghLog := originWithGh(t, a)
	return a, ghLog
}

// The stacked PRs prs opens follow the template too (#361).
func TestPRsFollowRepoTemplate(t *testing.T) {
	t.Parallel()
	a, ghLog := prsSetup(t, quarkTemplate)
	tk, err := a.Spawn(SpawnReq{ID: "t1", Title: "Add login", Issue: 3123})
	must(t, err)
	write(t, tk.Worktree, "login.go", "package x\n")
	write(t, tk.Worktree, "login_test.go", "package x\n")
	commitAll(t, tk.Worktree, "feat: add login")
	must(t, a.Done(tk.ID, "Adds a login handler."))
	if _, err := a.Land(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	call := onlyCall(prCreate(ghLog(), got.Branch))
	if call == "" {
		t.Fatalf("no pr create for %s:\n%v", got.Branch, ghLog())
	}
	checkQuarkBody(t, call, "#3123")
	for _, want := range []string{"Adds a login handler.", "- [x] Feature\n", "- feat: add login\n"} {
		if !strings.Contains(call, want) {
			t.Fatalf("body lacks %q:\n%s", want, call)
		}
	}
}

// The publish path follows a strict template as well.
func TestPublishFollowsQuarkTemplate(t *testing.T) {
	t.Parallel()
	a, ghLog := prsSetup(t, quarkTemplate)
	tk, err := a.Spawn(SpawnReq{ID: "t1", Title: "Fix login", Issue: 3128})
	must(t, err)
	write(t, tk.Worktree, "login.go", "package x\n")
	commitAll(t, tk.Worktree, "fix: login crash")
	must(t, a.Done(tk.ID, "Stops the crash."))
	if _, err := a.Land(); err != nil {
		t.Fatal(err)
	}
	res, err := a.Publish(PublishReq{Target: tk.ID})
	must(t, err)
	call := onlyCall(prCreate(ghLog(), res.Branch))
	checkQuarkBody(t, call, "#3128")
	if !strings.Contains(call, "- [x] Bug fix\n") || !strings.Contains(call, "Published by `saddle publish`") {
		t.Fatalf("publish body:\n%s", call)
	}
}

// Without a template, prs still sends the task's summary as the body.
func TestPRsWithoutTemplateSendSummary(t *testing.T) {
	t.Parallel()
	a, ghLog := prsSetup(t, "")
	tk := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	call := onlyCall(prCreate(ghLog(), tk.Branch))
	if !strings.HasSuffix(strings.TrimSpace(call), "--body one summary\n\nSaddle-Task: t1") {
		t.Fatalf("body isn't the summary and the task's trailer: %q", call)
	}
}
