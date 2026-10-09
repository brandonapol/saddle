package docs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/cli"
)

// repo is the repository root, relative to this package.
const repo = "../.."

const glossaryPath = "docs/GLOSSARY.md"

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var (
	entryRe = regexp.MustCompile("(?m)^### (.+)$")
	tickRe  = regexp.MustCompile("`([^`]+)`")
)

// glossary is docs/GLOSSARY.md: its entry headings, in order, the backticked
// terms those headings define, and the whole text with every run of
// whitespace as one space, so a name wrapped across lines still counts.
type glossary struct {
	entries []string
	terms   map[string]bool
	text    string
}

func readGlossary(t *testing.T) glossary {
	t.Helper()
	md := read(t, glossaryPath)
	g := glossary{terms: map[string]bool{}, text: strings.Join(strings.Fields(md), " ")}
	for _, m := range entryRe.FindAllStringSubmatch(md, -1) {
		g.entries = append(g.entries, m[1])
		for _, tm := range tickRe.FindAllStringSubmatch(m[1], -1) {
			g.terms[tm[1]] = true
		}
	}
	if len(g.entries) == 0 {
		t.Fatalf("%s has no ### entries", glossaryPath)
	}
	return g
}

// Every command `saddle help` lists has its own entry, headed `saddle <name>`,
// and every visible subcommand is named in the glossary as `saddle <cmd> <sub>`.
func TestGlossaryCoversEveryCommand(t *testing.T) {
	g := readGlossary(t)
	root := cli.Root()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	n := 0
	for _, c := range root.Commands() {
		if !c.IsAvailableCommand() && c.Name() != "help" {
			continue
		}
		n++
		if term := "saddle " + c.Name(); !g.terms[term] {
			t.Errorf("%s has no entry headed `%s`: add one (%q)", glossaryPath, term, c.Short)
		}
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		var walk func(c *cobra.Command, path string)
		walk = func(c *cobra.Command, path string) {
			for _, s := range c.Commands() {
				if !s.IsAvailableCommand() {
					continue
				}
				p := path + " " + s.Name()
				if !strings.Contains(g.text, p) {
					t.Errorf("%s never names `%s`: mention it under `%s` (%q)", glossaryPath, p, path, s.Short)
				}
				walk(s, p)
			}
		}
		walk(c, "saddle "+c.Name())
	}
	if n < 30 {
		t.Fatalf("found only %d commands; is cli.Root() wired up?", n)
	}
}

// Every task status and train state the code defines has an entry headed
// with the state's literal, e.g. `needs_you`.
func TestGlossaryCoversEveryState(t *testing.T) {
	g := readGlossary(t)
	states := codeStates(t)
	if len(states) < 15 {
		t.Fatalf("found only %d states (%v); did the consts move?", len(states), states)
	}
	for _, s := range states {
		if !g.terms[s] {
			t.Errorf("%s has no entry headed `%s`: every task and train state needs one", glossaryPath, s)
		}
	}
}

// codeStates collects the task statuses and train entry states: the
// const blocks store.go documents as such, plus internal/app's Status* and
// Train* string consts.
func codeStates(t *testing.T) []string {
	t.Helper()
	var out []string
	add := func(v string) {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(repo, "internal/store/store.go"), nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	blocks := 0
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST || gd.Doc == nil {
			continue
		}
		doc := gd.Doc.Text()
		if !strings.HasPrefix(doc, "Task statuses") && !strings.HasPrefix(doc, "Train entry states") {
			continue
		}
		blocks++
		for _, sp := range gd.Specs {
			for _, v := range sp.(*ast.ValueSpec).Values {
				if s, ok := stringLit(v); ok {
					add(s)
				}
			}
		}
	}
	if blocks != 2 {
		t.Fatalf("store.go: found %d of the 2 state const blocks (\"Task statuses\", \"Train entry states\")", blocks)
	}
	pkgs, err := filepath.Glob(filepath.Join(repo, "internal/app/*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkgs {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) || !isStateName(name.Name) {
						continue
					}
					if s, ok := stringLit(vs.Values[i]); ok {
						add(s)
					}
				}
			}
		}
	}
	return out
}

// isStateName matches StatusPaused or TrainMerged, not TrainLockFile.
func isStateName(n string) bool {
	for _, p := range []string{"Status", "Train"} {
		rest, ok := strings.CutPrefix(n, p)
		if ok && rest != "" && strings.ToUpper(rest[:1]) == rest[:1] && !strings.Contains(rest, "Lock") {
			return true
		}
	}
	return false
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	return s, err == nil
}

// Every MCP tool an agent or the orchestrator can call is named, backticked,
// somewhere in the glossary.
func TestGlossaryCoversEveryMCPTool(t *testing.T) {
	g := readGlossary(t)
	src := read(t, "internal/mcpserver/server.go")
	tools := regexp.MustCompile(`mcp\.Tool\{Name:\s*"([a-z_]+)"`).FindAllStringSubmatch(src, -1)
	if len(tools) < 15 {
		t.Fatalf("found only %d MCP tools in server.go; did registration move?", len(tools))
	}
	for _, m := range tools {
		if !strings.Contains(g.text, "`"+m[1]+"`") {
			t.Errorf("%s never names the MCP tool `%s`", glossaryPath, m[1])
		}
	}
}

// Entries are alphabetical, ignoring backticks, a leading "saddle " and case,
// so the glossary reads as a dictionary.
func TestGlossaryIsAlphabetical(t *testing.T) {
	g := readGlossary(t)
	for i := 1; i < len(g.entries); i++ {
		if sortKey(g.entries[i-1]) > sortKey(g.entries[i]) {
			t.Errorf("entry %q comes before %q; keep entries alphabetical", g.entries[i-1], g.entries[i])
		}
	}
}

func sortKey(h string) string {
	h = strings.ToLower(strings.ReplaceAll(h, "`", ""))
	return strings.TrimPrefix(h, "saddle ")
}

// The README's vocabulary section sits above the quickstart, explains land
// and restack, and links the glossary.
func TestReadmeVocabularySection(t *testing.T) {
	r := read(t, "README.md")
	vocab := strings.Index(r, "\n## How Saddle works")
	quick := strings.Index(r, "\n## Quickstart")
	if vocab < 0 || quick < 0 || vocab > quick {
		t.Fatalf("README needs a \"## How Saddle works\" section before \"## Quickstart\" (at %d, %d)", vocab, quick)
	}
	section := r[vocab:quick]
	for _, want := range []string{"**land**", "**restack**", "(docs/GLOSSARY.md)", "never pushes"} {
		if !strings.Contains(section, want) {
			t.Errorf("README vocabulary section lacks %q", want)
		}
	}
}

var linkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// Relative links in the README and the docs point at files that exist, and
// their #anchors at headings in those files.
func TestDocLinksResolve(t *testing.T) {
	docs, err := filepath.Glob(filepath.Join(repo, "docs/*.md"))
	if err != nil {
		t.Fatal(err)
	}
	files := []string{"README.md"}
	for _, d := range docs {
		rel, _ := filepath.Rel(repo, d)
		files = append(files, rel)
	}
	for _, file := range files {
		for _, m := range linkRe.FindAllStringSubmatch(read(t, file), -1) {
			link := m[1]
			if strings.Contains(link, "://") || strings.HasPrefix(link, "mailto:") {
				continue
			}
			path, anchor, _ := strings.Cut(link, "#")
			target := file
			if path != "" {
				target = filepath.Join(filepath.Dir(file), path)
				if _, err := os.Stat(filepath.Join(repo, target)); err != nil {
					t.Errorf("%s links %s, which does not exist", file, link)
					continue
				}
			}
			if anchor == "" || !strings.HasSuffix(target, ".md") {
				continue
			}
			if !slices.Contains(anchors(read(t, target)), anchor) {
				t.Errorf("%s links %s, but %s has no heading with anchor #%s", file, link, target, anchor)
			}
		}
	}
}

var headingRe = regexp.MustCompile("(?m)^#{1,6} (.+)$")

// anchors are the GitHub anchors of md's headings: lower case, punctuation
// other than - and _ dropped, spaces as -, duplicates suffixed -1, -2, ...
func anchors(md string) []string {
	var out []string
	seen := map[string]int{}
	inFence := false
	for line := range strings.SplitSeq(md, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
		}
		m := headingRe.FindStringSubmatch(line)
		if inFence || m == nil {
			continue
		}
		var b strings.Builder
		for _, r := range strings.ToLower(m[1]) {
			switch {
			case r == ' ':
				b.WriteRune('-')
			case r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
				b.WriteRune(r)
			}
		}
		a := b.String()
		if n := seen[a]; n > 0 {
			out = append(out, a+"-"+strconv.Itoa(n))
		} else {
			out = append(out, a)
		}
		seen[a]++
	}
	return out
}
