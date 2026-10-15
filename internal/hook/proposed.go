package hook

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// proposed works out the file at abs before and after the tool call in, from
// the file on disk and the tool input (#588). before is nil when the file
// does not exist yet. known is false when the call's effect on abs can't be
// computed: an unsupported tool, an old_string that is missing or occurs
// more than once without replace_all, or a patch hunk that doesn't apply.
func proposed(in Input, abs string) (before, after []byte, known bool) {
	before, err := os.ReadFile(abs)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false
	}
	ti := in.ToolInput
	if patch, _ := ti["patchText"].(string); patch != "" {
		s, ok := patchResult(patch, in.Cwd, abs)
		return before, []byte(s), ok
	}
	exists := before != nil
	cur := string(before)
	switch in.ToolName {
	case "Write", "write":
		s, ok := ti["content"].(string)
		return before, []byte(s), ok
	case "Edit", "edit":
		s, ok := applyEdit(cur, exists, ti)
		return before, []byte(s), ok
	case "MultiEdit", "multiedit":
		edits, _ := ti["edits"].([]any)
		if len(edits) == 0 {
			return before, nil, false
		}
		for _, e := range edits {
			m, _ := e.(map[string]any)
			s, ok := applyEdit(cur, exists, m)
			if !ok {
				return before, nil, false
			}
			cur, exists = s, true
		}
		return before, []byte(cur), true
	}
	return before, nil, false
}

// applyEdit applies one Edit-shaped replacement (Claude's snake_case keys or
// opencode's camelCase) to cur. An empty old_string creates a missing file.
func applyEdit(cur string, exists bool, m map[string]any) (string, bool) {
	old, ok1 := str(m, "old_string", "oldString")
	repl, ok2 := str(m, "new_string", "newString")
	if !ok1 || !ok2 {
		return "", false
	}
	if old == "" {
		return repl, !exists
	}
	all, _ := m["replace_all"].(bool)
	if b, _ := m["replaceAll"].(bool); b {
		all = true
	}
	switch n := strings.Count(cur, old); {
	case n == 0, n > 1 && !all:
		return "", false
	case all:
		return strings.ReplaceAll(cur, old, repl), true
	default:
		return strings.Replace(cur, old, repl, 1), true
	}
}

func str(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s, true
		}
	}
	return "", false
}

// patchOp is one file section of an apply_patch patch.
type patchOp struct {
	kind string // "add", "update" or "delete"
	path string
	move string // update only: the file it is renamed to
	body []string
}

func parsePatch(patch string) []patchOp {
	var ops []patchOp
	for _, line := range strings.Split(strings.TrimSuffix(patch, "\n"), "\n") {
		header := func(prefix, kind string) bool {
			rest, ok := strings.CutPrefix(line, prefix)
			if ok {
				ops = append(ops, patchOp{kind: kind, path: strings.TrimSpace(rest)})
			}
			return ok
		}
		switch {
		case header("*** Add File:", "add"), header("*** Update File:", "update"), header("*** Delete File:", "delete"):
		case strings.HasPrefix(line, "*** Move to:"):
			if len(ops) > 0 {
				ops[len(ops)-1].move = strings.TrimSpace(strings.TrimPrefix(line, "*** Move to:"))
			}
		case line == "*** Begin Patch", line == "*** End Patch":
		default:
			if len(ops) > 0 {
				ops[len(ops)-1].body = append(ops[len(ops)-1].body, line)
			}
		}
	}
	return ops
}

// patchResult is the content of abs after patch: an added file's lines, an
// updated file with its hunks applied, or empty for a deleted file or one
// moved away. It is unknown when the patch doesn't touch abs exactly once.
func patchResult(patch, cwd, abs string) (string, bool) {
	resolve := func(p string) string {
		if p != "" && !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		return filepath.Clean(p)
	}
	abs = filepath.Clean(abs)
	ops := parsePatch(patch)
	var hit *patchOp
	for i := range ops {
		op := &ops[i]
		op.path = resolve(op.path)
		if op.move != "" {
			op.move = resolve(op.move)
		}
		if op.path != abs && op.move != abs {
			continue
		}
		if hit != nil {
			return "", false
		}
		hit = op
	}
	switch {
	case hit == nil:
		return "", false
	case hit.kind == "add":
		var lines []string
		for _, l := range hit.body {
			s, ok := strings.CutPrefix(l, "+")
			if !ok {
				return "", false
			}
			lines = append(lines, s)
		}
		return joinLines(lines), true
	case hit.kind == "delete":
		return "", true
	case hit.move != "" && hit.move != abs:
		return "", true // moved away
	}
	src, err := os.ReadFile(hit.path)
	if err != nil {
		return "", false
	}
	return applyHunks(string(src), hit.body)
}

// applyHunks applies an Update File body to src. Each hunk starts at an @@
// line, optionally naming a line to find first; its context and removed
// lines must then appear, in order, after the previous hunk.
func applyHunks(src string, body []string) (string, bool) {
	lines := strings.Split(strings.TrimSuffix(src, "\n"), "\n")
	if src == "" {
		lines = nil
	}
	type hunk struct {
		anchor   string
		old, new []string
		eof      bool
	}
	var hunks []hunk
	for _, l := range body {
		if rest, ok := strings.CutPrefix(l, "@@"); ok {
			hunks = append(hunks, hunk{anchor: strings.TrimSpace(rest)})
			continue
		}
		if len(hunks) == 0 {
			hunks = append(hunks, hunk{})
		}
		h := &hunks[len(hunks)-1]
		switch {
		case l == "*** End of File":
			h.eof = true
		case l == "":
			h.old, h.new = append(h.old, ""), append(h.new, "")
		case l[0] == ' ':
			h.old, h.new = append(h.old, l[1:]), append(h.new, l[1:])
		case l[0] == '-':
			h.old = append(h.old, l[1:])
		case l[0] == '+':
			h.new = append(h.new, l[1:])
		default:
			return "", false
		}
	}
	cursor := 0
	for _, h := range hunks {
		if h.anchor != "" {
			i := find(lines, []string{h.anchor}, cursor)
			if i < 0 {
				return "", false
			}
			cursor = i + 1
		}
		at := len(lines)
		if len(h.old) > 0 {
			if end := len(lines) - len(h.old); h.eof && end >= cursor && find(lines[end:], h.old, 0) == 0 {
				at = end
			} else {
				at = find(lines, h.old, cursor)
			}
			if at < 0 {
				return "", false
			}
		}
		lines = append(lines[:at:at], append(append([]string{}, h.new...), lines[at+len(h.old):]...)...)
		cursor = at + len(h.new)
	}
	return joinLines(lines), true
}

// find is the index of the first run of want in lines at or after from, or -1.
func find(lines, want []string, from int) int {
	for i := from; i+len(want) <= len(lines); i++ {
		match := true
		for j, w := range want {
			if lines[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// applied works out the file at abs before and after a tool call that has
// run (PostToolUse): after is the file on disk, before the original Claude
// Code reports in tool_response. Without that, it is unknown.
func applied(in Input, abs string) (before, after []byte, known bool) {
	after, err := os.ReadFile(abs)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, false
	}
	orig, ok := in.ToolResponse["originalFile"]
	if !ok {
		return nil, after, false
	}
	switch o := orig.(type) {
	case nil:
		return nil, after, true // the call created the file
	case string:
		return []byte(o), after, true
	}
	return nil, after, false
}
