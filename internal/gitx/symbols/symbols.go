// Package symbols extracts functions and types from source files with
// tree-sitter, for the git watcher's touched-symbol tracking. It uses
// gotreesitter, a pure-Go runtime, so the build stays CGO-free.
//
// Adding a language is one Lang value: a grammar, the file extensions, and
// which node kinds are symbols.
package symbols

import (
	"path/filepath"
	"strings"

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

// Symbols parses src and returns its symbols, outer before inner. Syntax
// errors are tolerated: agents save half-written files.
func (x *Extractor) Symbols(path string, src []byte) ([]gitx.Symbol, error) {
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
	var out []gitx.Symbol
	l.walk(Node{Node: tree.RootNode(), Lang: lang, Src: src}, "", &out)
	return out, nil
}

func (l *Lang) walk(n Node, outer string, out *[]gitx.Symbol) {
	for i := 0; i < n.NamedChildCount(); i++ {
		c := Node{Node: n.NamedChild(i), Lang: n.Lang, Src: n.Src}
		typ := c.Type(c.Lang)
		qual := outer
		if kind, ok := l.Kinds[typ]; ok {
			if name := l.nameOf(c); name != "" {
				qual = l.qualify(outer, name)
				*out = append(*out, gitx.Symbol{
					Name:      qual,
					Kind:      kind,
					StartLine: int(c.StartPoint().Row) + 1,
					EndLine:   endLine(c.Node),
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
