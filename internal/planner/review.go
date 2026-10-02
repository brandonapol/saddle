package planner

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// Doc is a plan under review, saved as TOML so a human can edit it.
type Doc struct {
	Epic     string // epic title
	Source   string // the epic source argument, e.g. "gh:#12"
	Repo     string // GitHub repo of the epic issue, "" for the current repo
	Issue    int    // epic issue number, 0 when there is none
	Text     string // epic text, kept for re-planning
	Approved bool
	Base     string // base commit frozen on approval
	Tasks    []Task
}

// ErrApproved is returned when changing a plan that has been approved.
// Reopen it first.
var ErrApproved = errors.New("plan is approved and frozen; reopen it to change it")

type docFile struct {
	Epic     string    `toml:"epic"`
	Source   string    `toml:"source"`
	Repo     string    `toml:"repo"`
	Issue    int       `toml:"issue"`
	Approved bool      `toml:"approved"`
	Base     string    `toml:"base"`
	Text     string    `toml:"text"`
	Tasks    []docTask `toml:"task"`
}

type docTask struct {
	ID       string   `toml:"id"`
	Title    string   `toml:"title"`
	Goal     string   `toml:"goal"`
	Claims   []string `toml:"claims"`
	Deps     []string `toml:"deps"`
	DoneWhen []string `toml:"done_when"`
	Barrier  bool     `toml:"barrier"`
	Model    string   `toml:"model"`
	Adapter  string   `toml:"adapter"`
	Issues   []string `toml:"issues"`
}

// Render writes d as TOML. A comment header shows the checker's result for
// serial and limit: the waves, train routes, barriers and edges, or why the
// check failed.
func Render(d Doc, serial []string, limit int) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# saddle plan: %s\n", d.Epic)
	if d.Approved {
		fmt.Fprintf(&b, "# Approved and frozen at base %s. `saddle plan reopen` to change it.\n", d.Base)
	} else {
		b.WriteString("# Edit with `saddle plan edit`: the checker re-runs on save. `saddle plan approve` freezes it.\n")
	}
	b.WriteString("#\n")
	if err := Validate(d.Tasks); err != nil {
		writeComment(&b, "check failed: "+err.Error())
	} else if p, err := Check(d.Tasks, serial, limit); err != nil {
		writeComment(&b, "check failed: "+err.Error())
	} else {
		for i, w := range p.Waves {
			fmt.Fprintf(&b, "# wave %d: %s\n", i+1, strings.Join(w, ", "))
		}
		for _, f := range p.Train {
			fmt.Fprintf(&b, "# train: %s: %s\n", f.Task, f.Reason)
		}
		for _, f := range p.Barriers {
			fmt.Fprintf(&b, "# barrier: %s: %s\n", f.Task, f.Reason)
		}
		for _, e := range p.Edges {
			fmt.Fprintf(&b, "# edge: %s -> %s: %s\n", e.From, e.To, e.Reason)
		}
	}
	b.WriteString("\n")
	kv(&b, "epic", tomlString(d.Epic))
	kv(&b, "source", tomlString(d.Source))
	if d.Repo != "" {
		kv(&b, "repo", tomlString(d.Repo))
	}
	if d.Issue != 0 {
		kv(&b, "issue", fmt.Sprint(d.Issue))
	}
	kv(&b, "approved", fmt.Sprint(d.Approved))
	if d.Base != "" {
		kv(&b, "base", tomlString(d.Base))
	}
	kv(&b, "text", tomlText(d.Text))
	for _, t := range d.Tasks {
		b.WriteString("\n[[task]]\n")
		kv(&b, "id", tomlString(t.ID))
		kv(&b, "title", tomlString(t.Title))
		kv(&b, "goal", tomlText(t.Plan))
		kv(&b, "claims", tomlArray(t.Claims))
		if len(t.After) > 0 {
			kv(&b, "deps", tomlArray(t.After))
		}
		kv(&b, "done_when", tomlArray(t.DoneWhen))
		if t.Barrier {
			kv(&b, "barrier", "true")
		}
		if t.Model != "" {
			kv(&b, "model", tomlString(t.Model))
		}
		if t.Adapter != "" {
			kv(&b, "adapter", tomlString(t.Adapter))
		}
		if len(t.Issues) > 0 {
			kv(&b, "issues", tomlArray(t.Issues))
		}
	}
	return []byte(b.String())
}

// Decode parses a rendered plan strictly: unknown keys are errors and the
// tasks must pass Validate.
func Decode(b []byte) (Doc, error) {
	var f docFile
	md, err := toml.Decode(string(b), &f)
	if err != nil {
		return Doc{}, fmt.Errorf("parse plan: %w", err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		keys := make([]string, len(u))
		for i, k := range u {
			keys[i] = k.String()
		}
		return Doc{}, fmt.Errorf("parse plan: unknown keys: %s", strings.Join(keys, ", "))
	}
	d := Doc{Epic: f.Epic, Source: f.Source, Repo: f.Repo, Issue: f.Issue, Text: f.Text, Approved: f.Approved, Base: f.Base}
	for _, t := range f.Tasks {
		d.Tasks = append(d.Tasks, Task{
			ID: strings.TrimSpace(t.ID), Title: strings.TrimSpace(t.Title), Plan: t.Goal,
			Claims: nonEmpty(t.Claims), After: nonEmpty(t.Deps), DoneWhen: nonEmpty(t.DoneWhen),
			Barrier: t.Barrier, Model: t.Model, Adapter: t.Adapter, Issues: nonEmpty(t.Issues),
		})
	}
	if err := Validate(d.Tasks); err != nil {
		return Doc{}, err
	}
	return d, nil
}

// LoadDoc reads and decodes the plan at path.
func LoadDoc(path string) (Doc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Doc{}, err
	}
	d, err := Decode(b)
	if err != nil {
		return Doc{}, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// WriteDoc renders d to path.
func WriteDoc(path string, d Doc, serial []string, limit int) error {
	return os.WriteFile(path, Render(d, serial, limit), 0o644)
}

// Editor lets a human edit the file at path, returning when they are done.
type Editor func(path string) error

// Edit opens the plan at path in edit, then decodes and checks the result.
// A good plan is re-rendered with a fresh check header. A bad one is left as
// the user wrote it, and the error says what is wrong, so they can edit again.
func Edit(path string, edit Editor, serial []string, limit int) (Doc, Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Doc{}, Plan{}, err
	}
	// Only the approved flag matters here: a plan left broken by an earlier
	// edit must still open, so the user can fix it.
	var frozen struct {
		Approved bool `toml:"approved"`
	}
	if _, err := toml.Decode(string(b), &frozen); err == nil && frozen.Approved {
		return Doc{}, Plan{}, ErrApproved
	}
	if err := edit(path); err != nil {
		return Doc{}, Plan{}, fmt.Errorf("editor: %w", err)
	}
	d, err := LoadDoc(path)
	if err != nil {
		return Doc{}, Plan{}, err
	}
	p, err := Check(d.Tasks, serial, limit)
	if err != nil {
		return d, p, err
	}
	return d, p, WriteDoc(path, d, serial, limit)
}

// Approve checks the plan at path and freezes it at base. A plan that fails
// the checker is not approved.
func Approve(path, base string, serial []string, limit int) (Doc, error) {
	d, err := LoadDoc(path)
	if err != nil {
		return Doc{}, err
	}
	if d.Approved {
		return d, ErrApproved
	}
	if _, err := Check(d.Tasks, serial, limit); err != nil {
		return d, err
	}
	d.Approved, d.Base = true, base
	return d, WriteDoc(path, d, serial, limit)
}

// Reopen unfreezes an approved plan so it can be edited again.
func Reopen(path string, serial []string, limit int) (Doc, error) {
	d, err := LoadDoc(path)
	if err != nil {
		return Doc{}, err
	}
	d.Approved, d.Base = false, ""
	return d, WriteDoc(path, d, serial, limit)
}

func kv(b *strings.Builder, k, v string) { fmt.Fprintf(b, "%s = %s\n", k, v) }

func writeComment(b *strings.Builder, s string) {
	for line := range strings.Lines(s) {
		fmt.Fprintf(b, "# %s\n", strings.TrimRight(line, "\n"))
	}
}

// tomlString quotes s as a TOML basic string.
func tomlString(s string) string { return `"` + tomlEscape(s, false) + `"` }

// tomlText quotes s as a multi-line basic string when it spans lines, so
// goals stay readable in an editor.
func tomlText(s string) string {
	if !strings.Contains(s, "\n") {
		return tomlString(s)
	}
	return "\"\"\"\n" + tomlEscape(s, true) + `"""`
}

func tomlEscape(s string, multiline bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n' && multiline:
			b.WriteByte('\n')
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func tomlArray(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = tomlString(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}
