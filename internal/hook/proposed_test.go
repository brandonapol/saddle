package hook

import (
	"os"
	"path/filepath"
	"testing"
)

// #588: the hook works out each written file's content after the call from
// the file on disk and the tool input. What it can't work out is unknown.
func TestProposedContent(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "f.go")
	missing := filepath.Join(dir, "new.go")
	const disk = "package f\n\nfunc A() {}\n\nfunc B() {}\n"
	edit := func(old, new string, all bool) map[string]any {
		return map[string]any{"old_string": old, "new_string": new, "replace_all": all}
	}
	for _, tc := range []struct {
		name  string
		tool  string
		path  string // defaults to abs
		input map[string]any
		want  string
		known bool
	}{
		{name: "write", tool: "Write", input: map[string]any{"file_path": abs, "content": "x\n"}, want: "x\n", known: true},
		{name: "write new file", tool: "Write", path: missing, input: map[string]any{"file_path": missing, "content": "y\n"}, want: "y\n", known: true},
		{name: "opencode write", tool: "write", input: map[string]any{"filePath": abs, "content": "z\n"}, want: "z\n", known: true},
		{name: "edit once", tool: "Edit", input: with(edit("func A() {}", "func A() int { return 1 }", false), abs),
			want: "package f\n\nfunc A() int { return 1 }\n\nfunc B() {}\n", known: true},
		{name: "edit replace_all", tool: "Edit", input: with(edit("() {}", "() {} // x", true), abs),
			want: "package f\n\nfunc A() {} // x\n\nfunc B() {} // x\n", known: true},
		{name: "edit ambiguous", tool: "Edit", input: with(edit("() {}", "()", false), abs)},
		{name: "edit missing", tool: "Edit", input: with(edit("func C", "func D", false), abs)},
		{name: "edit missing file", tool: "Edit", path: missing, input: with(edit("a", "b", false), missing)},
		{name: "edit creates file", tool: "Edit", path: missing, input: with(edit("", "package n\n", false), missing), want: "package n\n", known: true},
		{name: "opencode edit", tool: "edit", input: map[string]any{"filePath": abs, "oldString": "B", "newString": "C"},
			want: "package f\n\nfunc A() {}\n\nfunc C() {}\n", known: true},
		{name: "multiedit in order", tool: "MultiEdit", input: map[string]any{"file_path": abs, "edits": []any{
			edit("func A", "func X", false),
			edit("func X() {}", "func X() { B() }", false),
		}}, want: "package f\n\nfunc X() { B() }\n\nfunc B() {}\n", known: true},
		{name: "multiedit second edit ambiguous", tool: "MultiEdit", input: map[string]any{"file_path": abs, "edits": []any{
			edit("func A", "func B", false),
			edit("func B", "func C", false),
		}}},
		{name: "patch update", tool: "apply_patch", input: map[string]any{"patchText": "*** Begin Patch\n*** Update File: " + abs +
			"\n@@\n func A() {}\n \n-func B() {}\n+func B() int { return 2 }\n*** End Patch\n"},
			want: "package f\n\nfunc A() {}\n\nfunc B() int { return 2 }\n", known: true},
		{name: "patch update with context header", tool: "apply_patch", input: map[string]any{"patchText": "*** Begin Patch\n*** Update File: " + abs +
			"\n@@ func A() {}\n-func B() {}\n+func Z() {}\n*** End of File\n*** End Patch\n"},
			want: "package f\n\nfunc A() {}\n\nfunc Z() {}\n", known: true},
		{name: "patch hunk not found", tool: "apply_patch", input: map[string]any{"patchText": "*** Begin Patch\n*** Update File: " + abs +
			"\n@@\n-func Q() {}\n+func R() {}\n*** End Patch\n"}},
		{name: "patch add", tool: "apply_patch", path: missing, input: map[string]any{"patchText": "*** Begin Patch\n*** Add File: " + missing +
			"\n+package n\n+\n+func N() {}\n*** End Patch\n"}, want: "package n\n\nfunc N() {}\n", known: true},
		{name: "patch delete", tool: "apply_patch", input: map[string]any{"patchText": "*** Begin Patch\n*** Delete File: " + abs + "\n*** End Patch\n"},
			want: "", known: true},
		{name: "patch move source", tool: "apply_patch", input: map[string]any{"patchText": "*** Begin Patch\n*** Update File: " + abs +
			"\n*** Move to: " + missing + "\n@@\n-func B() {}\n+func M() {}\n*** End Patch\n"}, want: "", known: true},
		{name: "patch move dest", tool: "apply_patch", path: missing, input: map[string]any{"patchText": "*** Begin Patch\n*** Update File: " + abs +
			"\n*** Move to: " + missing + "\n@@\n-func B() {}\n+func M() {}\n*** End Patch\n"},
			want: "package f\n\nfunc A() {}\n\nfunc M() {}\n", known: true},
		{name: "patch other file", tool: "apply_patch", input: map[string]any{"patchText": "*** Begin Patch\n*** Add File: " + missing + "\n+x\n*** End Patch\n"}},
		{name: "notebook", tool: "NotebookEdit", input: map[string]any{"notebook_path": abs, "new_source": "x"}},
		{name: "unknown tool", tool: "Frobnicate", input: map[string]any{"file_path": abs, "content": "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(abs, []byte(disk), 0o644); err != nil {
				t.Fatal(err)
			}
			p := tc.path
			if p == "" {
				p = abs
			}
			before, after, known := proposed(Input{ToolName: tc.tool, ToolInput: tc.input, Cwd: dir}, p)
			if known != tc.known {
				t.Fatalf("known = %v, want %v (after %q)", known, tc.known, after)
			}
			if p == abs && string(before) != disk {
				t.Fatalf("before = %q, want the file on disk", before)
			}
			if p == missing && before != nil {
				t.Fatalf("before = %q for a missing file, want nil", before)
			}
			if known && string(after) != tc.want {
				t.Fatalf("after = %q, want %q", after, tc.want)
			}
		})
	}
}

// A relative patch path resolves against the payload's cwd, as writePaths does.
func TestProposedPatchRelativePath(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "sub", "f.txt")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := Input{ToolName: "apply_patch", Cwd: dir, ToolInput: map[string]any{
		"patchText": "*** Begin Patch\n*** Update File: sub/f.txt\n@@\n a\n-b\n+c\n*** End Patch\n"}}
	if _, after, known := proposed(in, abs); !known || string(after) != "a\nc\n" {
		t.Fatalf("after = %q, known = %v", after, known)
	}
}

func with(m map[string]any, path string) map[string]any {
	m["file_path"] = path
	return m
}
