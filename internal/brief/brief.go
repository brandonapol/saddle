// Package brief builds the compact view of one agent's task that saddle
// brief prints and the TUI's brief pane shows: its goal, the paths it owns,
// the paths it must leave alone, its done-when checklist and its children.
package brief

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// Brief is one task's compact brief.
type Brief struct {
	ID, Title, Status string
	Parent, Branch    string
	Train, PR         string
	Goal              string
	Owned             []string // the task's claims
	DoNotTouch        []string // serial files and other live tasks' claims, as "glob (owner)"
	DoneWhen          []Check
	Children          []Child
}

// Check is one done-when item; Checked when the prompt ticked it.
type Check struct {
	Text    string
	Checked bool
}

// Child is a task the brief's task spawned.
type Child struct{ ID, Title, Status string }

// maxHandsOff caps the do-not-touch list; the rest collapse to "+N more".
const maxHandsOff = 8

// Load builds the brief for task id from the store alone: it asks neither
// git nor GitHub, so it is cheap enough to poll.
func Load(a *app.App, id string) (Brief, error) {
	t, err := a.Store.Task(id)
	if err != nil {
		return Brief{}, err
	}
	ts, err := mcpserver.Tasks(a)
	if err != nil {
		return Brief{}, err
	}
	return Build(t.ID, t.Prompt, ts, a.Cfg.Serial)
}

// Build assembles the brief for task id from its prompt, every task and the
// repo's serial files.
func Build(id, prompt string, tasks []mcpserver.TaskView, serial []string) (Brief, error) {
	i := slices.IndexFunc(tasks, func(t mcpserver.TaskView) bool { return t.ID == id })
	if i < 0 {
		return Brief{}, fmt.Errorf("task %s: %w", id, store.ErrNotFound)
	}
	t := tasks[i]
	b := Brief{ID: t.ID, Title: t.Title, Status: t.Status, Parent: t.Parent, Branch: t.Branch, Train: t.Train, PR: t.PR,
		Goal: goal(prompt), Owned: slices.Clone(t.Claims), DoneWhen: doneWhen(prompt)}
	if b.Goal == "" {
		b.Goal = t.Title
	}
	b.DoNotTouch = slices.Clone(serial)
	for _, o := range tasks {
		if o.ID == id || o.Status == store.Landed || o.Status == store.Killed {
			continue
		}
		for _, g := range o.Claims {
			b.DoNotTouch = append(b.DoNotTouch, g+" ("+o.ID+")")
		}
	}
	slices.Sort(b.DoNotTouch)
	for _, o := range tasks {
		if o.Parent == id {
			b.Children = append(b.Children, Child{ID: o.ID, Title: o.Title, Status: o.Status})
		}
	}
	return b, nil
}

// goal is the first sentence of the prompt's first paragraph of prose.
func goal(prompt string) string {
	var para []string
	for _, l := range strings.Split(prompt, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "" && len(para) > 0:
			return firstSentence(strings.Join(para, " "))
		case l == "", strings.HasPrefix(l, "#"):
		default:
			para = append(para, l)
		}
	}
	return firstSentence(strings.Join(para, " "))
}

func firstSentence(s string) string {
	if ss := sentences(s); len(ss) > 0 {
		return ss[0]
	}
	return ""
}

// sentenceEnd ends a sentence at . ! or ? followed by whitespace.
var sentenceEnd = regexp.MustCompile(`[.!?]["')]*\s+`)

func sentences(s string) []string {
	s = strings.Join(strings.Fields(s), " ")
	var out []string
	for s != "" {
		loc := sentenceEnd.FindStringIndex(s)
		if loc == nil {
			out = append(out, s)
			break
		}
		out = append(out, strings.TrimSpace(s[:loc[1]]))
		s = s[loc[1]:]
	}
	return out
}

var (
	listItem  = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(.*)$`)
	checkbox  = regexp.MustCompile(`^\[([ xX])\]\s*(.*)$`)
	doneTitle = regexp.MustCompile(`(?i)done when|definition of done|acceptance`)
	mustWord  = regexp.MustCompile(`(?i)\bmust\b`)
)

// maxDoneWhen caps the sentences guessed from prose.
const maxDoneWhen = 5

// doneWhen reads the prompt's checklist: the list under a "Done when" (or
// acceptance) heading, else every checkbox item, else sentences that say
// what must hold.
func doneWhen(prompt string) []Check {
	lines := strings.Split(prompt, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if !(strings.HasPrefix(t, "#") || strings.HasSuffix(t, ":")) || !doneTitle.MatchString(t) {
			continue
		}
		var out []Check
		for _, l := range lines[i+1:] {
			t := strings.TrimSpace(l)
			if strings.HasPrefix(t, "#") || (t == "" && len(out) > 0) {
				break
			}
			if m := listItem.FindStringSubmatch(l); m != nil {
				out = append(out, item(m[1]))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	var out []Check
	for _, l := range lines {
		if m := listItem.FindStringSubmatch(l); m != nil && checkbox.MatchString(m[1]) {
			out = append(out, item(m[1]))
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, s := range sentences(prompt) {
		if mustWord.MatchString(s) && len(out) < maxDoneWhen {
			out = append(out, Check{Text: s})
		}
	}
	return out
}

func item(s string) Check {
	if m := checkbox.FindStringSubmatch(s); m != nil {
		return Check{Text: strings.TrimSpace(m[2]), Checked: m[1] != " "}
	}
	return Check{Text: strings.TrimSpace(s)}
}

// StatusLabel is a task status as words.
func StatusLabel(s string) string { return strings.ReplaceAll(s, "_", " ") }

// Lines renders the brief as plain lines no wider than w runes.
func (b Brief) Lines(w int) []string {
	w = max(w, 8)
	var out []string
	add := func(s string) { out = append(out, clip(s, w)) }
	add(b.ID + " " + b.Title)
	meta := []string{StatusLabel(b.Status)}
	if b.Train != "" && b.Status != store.Landed {
		meta = append(meta, "train "+b.Train)
	}
	if b.Parent != "" {
		meta = append(meta, "from "+b.Parent)
	}
	add(strings.Join(meta, " · "))
	if b.PR != "" {
		add(b.PR)
	}
	section := func(title string, items []string) {
		add(title)
		for _, it := range items {
			add("  " + it)
		}
	}
	add("Goal")
	for _, l := range wrap(b.Goal, w-2) {
		add("  " + l)
	}
	owned := b.Owned
	if len(owned) == 0 {
		owned = []string{"none yet (first write claims)"}
	}
	section("Owns", owned)
	off := b.DoNotTouch
	if len(off) > maxHandsOff {
		off = append(slices.Clone(off[:maxHandsOff]), fmt.Sprintf("+%d more", len(b.DoNotTouch)-maxHandsOff))
	}
	if len(off) == 0 {
		off = []string{"nothing"}
	}
	section("Hands off", off)
	if len(b.DoneWhen) > 0 {
		var items []string
		for _, c := range b.DoneWhen {
			box := "[ ] "
			if c.Checked {
				box = "[x] "
			}
			items = append(items, box+c.Text)
		}
		section("Done when", items)
	}
	if len(b.Children) > 0 {
		var items []string
		for _, c := range b.Children {
			items = append(items, c.ID+" "+StatusLabel(c.Status)+" "+c.Title)
		}
		section("Children", items)
	}
	return out
}

// String is the brief at 80 columns.
func (b Brief) String() string { return strings.Join(b.Lines(80), "\n") + "\n" }

// clip cuts s to w runes, ending in … when cut.
func clip(s string, w int) string {
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	return string([]rune(s)[:w-1]) + "…"
}

// wrap breaks s into lines of at most w runes at spaces; a word longer
// than w is clipped.
func wrap(s string, w int) []string {
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= w:
			line += " " + word
		default:
			out = append(out, clip(line, w))
			line = word
		}
	}
	if line != "" {
		out = append(out, clip(line, w))
	}
	return out
}
