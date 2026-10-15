// Package symbols extracts functions and types from source files with
// tree-sitter, for the git watcher's touched-symbol tracking. It uses
// gotreesitter, a pure-Go runtime, so the build stays CGO-free.
//
// Adding a language is one Lang value: a grammar, the file extensions, and
// which node kinds are symbols.
package symbols

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	ts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/brandonapol/saddle/internal/gitx"
)

// Lang describes how to find symbols in one language.
type Lang struct {
	Name     string
	Exts     []string                         // file extensions, with the dot
	Language func() *ts.Language              // the tree-sitter grammar
	Kinds    map[string]string                // node type -> symbol kind
	Leaf     map[string]bool                  // node types whose bodies are not searched
	NameOf   func(n Node) string              // optional; default is the "name" field
	Qualify  func(outer, inner string) string // optional; default "outer.inner"
}

// Node is a tree-sitter node with what NameOf needs to read it.
type Node struct {
	*ts.Node
	Lang *ts.Language
	Src  []byte
}

// Field returns the child in field name, or nil.
func (n Node) Field(name string) *ts.Node { return n.ChildByFieldName(name, n.Lang) }

// Text returns the source text of a child node.
func (n Node) Text(c *ts.Node) string { return c.Text(n.Src) }

// Extractor implements gitx.SymbolExtractor over a set of languages.
type Extractor struct {
	byExt map[string]*Lang
}

var _ gitx.SymbolExtractor = (*Extractor)(nil)

// New returns an extractor for langs.
func New(langs ...Lang) *Extractor {
	x := &Extractor{byExt: map[string]*Lang{}}
	for i := range langs {
		for _, e := range langs[i].Exts {
			x.byExt[e] = &langs[i]
		}
	}
	return x
}

// Default returns an extractor for every language this package knows.
func Default() *Extractor { return New(Go, Python) }

// Supports reports whether path has an extension the extractor knows.
func (x *Extractor) Supports(path string) bool {
	_, ok := x.byExt[filepath.Ext(path)]
	return ok
}

// Language returns the language name for path ("go", "python"), or "" when
// the extractor does not know the extension. The span cache keys on this,
// not the path, so a rename keeps the hit.
func (x *Extractor) Language(path string) string {
	l, ok := x.byExt[filepath.Ext(path)]
	if !ok {
		return ""
	}
	return l.Name
}

// Symbols parses src and returns its symbols, outer before inner. Syntax
// errors are tolerated: agents save half-written files.
func (x *Extractor) Symbols(path string, src []byte) ([]gitx.Symbol, error) {
	syms, _, err := x.Parse(path, src)
	return syms, err
}

// Parse returns the symbols in src and the line ranges of parse errors
// (tree-sitter ERROR nodes and missing nodes). A clean file has no error
// ranges. Callers fall back to a file-level check when an error range
// intersects a changed line. Signature rules are part of
// gitx.SpanCacheVersion: bump that constant when they change.
func (x *Extractor) Parse(path string, src []byte) ([]gitx.Symbol, []gitx.LineRange, error) {
	l, ok := x.byExt[filepath.Ext(path)]
	if !ok {
		return nil, nil, nil
	}
	lang := l.Language()
	tree, err := ts.NewParser(lang).Parse(src)
	if err != nil {
		return nil, nil, err
	}
	defer tree.Release()
	root := tree.RootNode()
	var out []gitx.Symbol
	l.walk(Node{Node: root, Lang: lang, Src: src}, "", &out)
	return out, errorRanges(root, lang), nil
}

func (l *Lang) walk(n Node, outer string, out *[]gitx.Symbol) {
	for i := 0; i < n.NamedChildCount(); i++ {
		c := Node{Node: n.NamedChild(i), Lang: n.Lang, Src: n.Src}
		typ := c.Type(c.Lang)
		qual := outer
		if kind, ok := l.Kinds[typ]; ok {
			if name := l.nameOf(c); name != "" {
				qual = l.qualify(outer, name)
				sig := signature(c)
				*out = append(*out, gitx.Symbol{
					Name:      qual,
					Kind:      kind,
					StartLine: int(c.StartPoint().Row) + 1,
					EndLine:   endLine(c.Node),
					SigHash:   sigHash(sig),
					SigText:   displaySig(sig),
				})
			}
		}
		if !l.Leaf[typ] {
			l.walk(c, qual, out)
		}
	}
}

func (l *Lang) nameOf(n Node) string {
	if l.NameOf != nil {
		return l.NameOf(n)
	}
	if f := n.Field("name"); f != nil {
		return n.Text(f)
	}
	return ""
}

func (l *Lang) qualify(outer, inner string) string {
	if l.Qualify != nil {
		return l.Qualify(outer, inner)
	}
	if outer == "" {
		return inner
	}
	return outer + "." + inner
}

// signature is the text hashed into SigHash. Whitespace is removed by sigHash.
//
// Go func/method: the declaration up to the body. Go type: the whole spec.
// Go var/const: names and the declared type, never the value. Python def:
// name, parameters and return annotation. Python class: name and superclasses.
func signature(n Node) string {
	switch n.Type(n.Lang) {
	case "function_declaration", "method_declaration":
		if body := n.Field("body"); body != nil {
			return slice(n.Src, int(n.StartByte()), int(body.StartByte()))
		}
		return slice(n.Src, int(n.StartByte()), int(n.EndByte()))
	case "type_spec", "type_alias":
		return slice(n.Src, int(n.StartByte()), int(n.EndByte()))
	case "var_spec", "const_spec":
		return spanFields(n, "name", "type")
	case "function_definition":
		return spanFields(n, "name", "parameters", "return_type")
	case "class_definition":
		return spanFields(n, "name", "superclasses")
	default:
		return slice(n.Src, int(n.StartByte()), int(n.EndByte()))
	}
}

// spanFields returns the source from the first listed field to the last, so
// tokens between them (commas, "->") stay in the signature and the body does not.
func spanFields(n Node, fields ...string) string {
	want := make(map[string]bool, len(fields))
	for _, f := range fields {
		want[f] = true
	}
	start, end := -1, -1
	for i := 0; i < n.ChildCount(); i++ {
		if !want[n.FieldNameForChild(i, n.Lang)] {
			continue
		}
		c := n.Child(i)
		if c == nil {
			continue
		}
		s, e := int(c.StartByte()), int(c.EndByte())
		if start < 0 || s < start {
			start = s
		}
		if e > end {
			end = e
		}
	}
	if start < 0 {
		return ""
	}
	return slice(n.Src, start, end)
}

func slice(src []byte, start, end int) string {
	if start < 0 || end > len(src) || start > end {
		return ""
	}
	return string(src[start:end])
}

func sigHash(text string) string {
	sum := sha256.Sum256([]byte(stripSpace(text)))
	return hex.EncodeToString(sum[:])
}

// displaySig is the signature as a note shows it: one line, internal
// whitespace collapsed. The hash uses stripSpace, so spacing still is not
// a signature change.
func displaySig(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Use is a name mentioned on a line. Qual is the selector's qualifier for
// an x.Name (s.Claim, store.Claim); it is empty for a bare identifier.
type Use struct {
	Name string
	Qual string
}

// Uses returns identifiers and x.Name selectors in src whose name starts
// on a line in lines (1-based). A selector is reported and its operands
// are also reported as bare names, so a same-package method call matches
// on the bare name. Callers drop names in strings and comments because
// those are not identifier nodes.
func (x *Extractor) Uses(path string, src []byte, lines map[int]bool) ([]Use, error) {
	if len(lines) == 0 {
		return nil, nil
	}
	l, ok := x.byExt[filepath.Ext(path)]
	if !ok {
		return nil, nil
	}
	lang := l.Language()
	tree, err := ts.NewParser(lang).Parse(src)
	if err != nil {
		return nil, err
	}
	defer tree.Release()
	var out []Use
	walkUses(tree.RootNode(), lang, src, lines, &out)
	return out, nil
}

func walkUses(n *ts.Node, lang *ts.Language, src []byte, lines map[int]bool, out *[]Use) {
	if n == nil {
		return
	}
	typ := n.Type(lang)
	if typ == "selector_expression" || typ == "attribute" {
		qualNode := n.ChildByFieldName("operand", lang)
		nameNode := n.ChildByFieldName("field", lang)
		if nameNode == nil {
			qualNode = n.ChildByFieldName("object", lang)
			nameNode = n.ChildByFieldName("attribute", lang)
		}
		if nameNode != nil && lines[int(nameNode.StartPoint().Row)+1] {
			*out = append(*out, Use{Name: nameNode.Text(src), Qual: qualIdent(qualNode, lang, src)})
		}
	}
	if isIdent(typ) && lines[int(n.StartPoint().Row)+1] {
		*out = append(*out, Use{Name: n.Text(src)})
	}
	for i := 0; i < n.ChildCount(); i++ {
		walkUses(n.Child(i), lang, src, lines, out)
	}
}

func qualIdent(n *ts.Node, lang *ts.Language, src []byte) string {
	if n == nil || !isIdent(n.Type(lang)) {
		return ""
	}
	return n.Text(src)
}

func isIdent(typ string) bool {
	switch typ {
	case "identifier", "field_identifier", "type_identifier", "package_identifier":
		return true
	default:
		return false
	}
}

func stripSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// errorRanges lists ERROR and missing nodes. A zero-width missing token is
// reported as the line it sits on.
func errorRanges(root *ts.Node, lang *ts.Language) []gitx.LineRange {
	var out []gitx.LineRange
	var walk func(*ts.Node)
	walk = func(n *ts.Node) {
		if n == nil {
			return
		}
		if n.IsError() || n.IsMissing() {
			out = append(out, gitx.LineRange{Start: int(n.StartPoint().Row) + 1, End: endLine(n)})
		}
		for i := 0; i < n.ChildCount(); i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	return mergeLineRanges(out)
}

func mergeLineRanges(rs []gitx.LineRange) []gitx.LineRange {
	if len(rs) == 0 {
		return nil
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Start != rs[j].Start {
			return rs[i].Start < rs[j].Start
		}
		return rs[i].End < rs[j].End
	})
	out := []gitx.LineRange{rs[0]}
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.Start <= last.End+1 {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

// endLine is the 1-based last line of n. A node ending at column 0 ends on
// the previous line (its trailing newline belongs to it).
func endLine(n *ts.Node) int {
	p := n.EndPoint()
	if p.Column == 0 && p.Row > n.StartPoint().Row {
		return int(p.Row)
	}
	return int(p.Row) + 1
}

// Go finds top-level functions, methods (as Recv.Method), types, vars and consts.
var Go = Lang{
	Name:     "go",
	Exts:     []string{".go"},
	Language: grammars.GoLanguage,
	Kinds: map[string]string{
		"function_declaration": "func",
		"method_declaration":   "method",
		"type_spec":            "type",
		"type_alias":           "type",
		"var_spec":             "var",
		"const_spec":           "const",
	},
	Leaf: map[string]bool{"function_declaration": true, "method_declaration": true, "type_spec": true, "type_alias": true},
	NameOf: func(n Node) string {
		switch n.Type(n.Lang) {
		case "method_declaration":
			name := n.Field("name")
			if name == nil {
				return ""
			}
			if recv := receiverType(n); recv != "" {
				return recv + "." + n.Text(name)
			}
			return n.Text(name)
		case "var_spec", "const_spec":
			// Several names can share one spec: `var a, b = 1, 2`.
			var names []string
			for i := 0; i < n.NamedChildCount(); i++ {
				c := n.NamedChild(i)
				if c.Type(n.Lang) == "identifier" {
					names = append(names, n.Text(c))
				}
			}
			return strings.Join(names, ",")
		}
		if f := n.Field("name"); f != nil {
			return n.Text(f)
		}
		return ""
	},
}

// receiverType returns the bare receiver type of a method: T for (t *T[K]).
func receiverType(n Node) string {
	recv := n.Field("receiver")
	if recv == nil {
		return ""
	}
	for i := 0; i < recv.NamedChildCount(); i++ {
		p := Node{Node: recv.NamedChild(i), Lang: n.Lang, Src: n.Src}
		if t := p.Field("type"); t != nil {
			s := strings.TrimLeft(p.Text(t), "*( ")
			if j := strings.IndexAny(s, "[) "); j >= 0 {
				s = s[:j]
			}
			return s
		}
	}
	return ""
}

// Python finds classes and functions, nested ones as Class.method.
var Python = Lang{
	Name:     "python",
	Exts:     []string{".py", ".pyi"},
	Language: grammars.PythonLanguage,
	Kinds: map[string]string{
		"class_definition":    "class",
		"function_definition": "func",
	},
}
