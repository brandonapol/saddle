package bench

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brandonapol/saddle/internal/forecast"
)

// Tree is a read-only view of a repo: the fixture on disk, or a branch.
type Tree interface {
	// Read returns a file's content and whether it exists.
	Read(rel string) (string, bool)
	// Files lists the files directly in dir ("" is the root).
	Files(dir string) []string
}

// DirTree is a Tree over a directory on disk.
type DirTree string

func (d DirTree) Read(rel string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(string(d), filepath.FromSlash(rel)))
	return string(b), err == nil
}

func (d DirTree) Files(dir string) []string {
	ents, _ := os.ReadDir(filepath.Join(string(d), filepath.FromSlash(dir)))
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, path.Join(dir, e.Name()))
		}
	}
	return out
}

// GitTree is a Tree over a commit of the repo at Dir.
type GitTree struct{ Dir, Rev string }

func (g GitTree) Read(rel string) (string, bool) {
	out, err := exec.Command("git", "-C", g.Dir, "show", g.Rev+":"+rel).Output()
	return string(out), err == nil
}

func (g GitTree) Files(dir string) []string {
	spec := g.Rev + ":" + dir
	if dir == "" {
		spec = g.Rev
	}
	out, err := exec.Command("git", "-C", g.Dir, "ls-tree", "--name-only", spec).Output()
	if err != nil {
		return nil
	}
	var fs []string
	for _, n := range strings.Fields(string(out)) {
		if _, ok := g.Read(path.Join(dir, n)); ok {
			fs = append(fs, path.Join(dir, n))
		}
	}
	return fs
}

// Change is what a ticket's edits do to a tree.
type Change struct {
	Writes  map[string]string // path -> new content
	Removes []string          // sorted
	Renames []forecast.Rename
	// Stale are edit paths whose anchor was in no file: the work the
	// ticket was written against is gone.
	Stale []string
	// Followed maps an edit's path to the file it was applied to instead,
	// because the anchor had moved there.
	Followed map[string]string
}

// Files is every path the change writes or removes, sorted.
func (c Change) Files() []string {
	out := append([]string(nil), c.Removes...)
	for p := range c.Writes {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Apply applies edits in order to base, as an agent reading the current
// tree would.
func Apply(base Tree, edits []Edit) Change {
	over := map[string]*string{} // nil: removed
	cur := func(p string) (string, bool) {
		if s, ok := over[p]; ok {
			if s == nil {
				return "", false
			}
			return *s, true
		}
		return base.Read(p)
	}
	set := func(p, s string) { over[p] = &s }
	c := Change{Writes: map[string]string{}, Followed: map[string]string{}}
	for _, e := range edits {
		p := path.Clean(filepath.ToSlash(e.Path))
		switch {
		case e.From != "":
			from := path.Clean(filepath.ToSlash(e.From))
			s, ok := cur(from)
			if !ok {
				c.Stale = append(c.Stale, from)
				continue
			}
			set(p, s)
			over[from] = nil
			c.Renames = append(c.Renames, forecast.Rename{Old: from, New: p})
		case e.Remove:
			over[p] = nil
		case e.Old != "":
			target := p
			if s, ok := cur(p); !ok || !strings.Contains(s, e.Old) {
				target = follow(base, over, path.Dir(p), e.Old, cur)
			}
			if target == "" {
				c.Stale = append(c.Stale, p)
				continue
			}
			s, _ := cur(target)
			set(target, strings.Replace(s, e.Old, e.New, 1))
			if target != p {
				c.Followed[p] = target
			}
		case e.Append:
			s, _ := cur(p)
			set(p, s+e.New)
		default:
			set(p, e.New)
		}
	}
	for p, s := range over {
		switch {
		case s != nil:
			c.Writes[p] = *s
		default:
			if _, ok := base.Read(p); ok {
				c.Removes = append(c.Removes, p)
			}
		}
	}
	sort.Strings(c.Removes)
	return c
}

// follow finds the file in dir that now holds old: the agent looks for
// the code it was asked to change after a move.
func follow(base Tree, over map[string]*string, dir, old string, cur func(string) (string, bool)) string {
	ls := dir
	if ls == "." {
		ls = ""
	}
	seen := map[string]bool{}
	var cands []string
	for _, f := range base.Files(ls) {
		seen[f] = true
		cands = append(cands, f)
	}
	for f := range over {
		if path.Dir(f) == dir && !seen[f] {
			cands = append(cands, f)
		}
	}
	sort.Strings(cands)
	for _, f := range cands {
		if s, ok := cur(f); ok && strings.Contains(s, old) {
			return f
		}
	}
	return ""
}
