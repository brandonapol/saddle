package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestLineDiffMatchesGit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	cases := []struct {
		name     string
		old, new string
	}{
		{"edit", "a\nb\nc\n", "a\nB\nc\n"},
		{"insert start", "b\nc\n", "A\nb\nc\n"},
		{"insert middle", "a\nc\n", "a\nB\nc\n"},
		{"insert end", "a\nb\n", "a\nb\nC\n"},
		{"delete", "a\nb\nc\n", "a\nc\n"},
		{"delete all", "a\nb\n", ""},
		{"empty to some", "", "a\nb\n"},
		{"both empty", "", ""},
		{"no trailing newline edit", "a\nb", "a\nB"},
		{"no trailing newline same", "a\nb", "a\nb"},
		{"newline versus none", "a\nb\n", "a\nb"},
		{"insert without trailing newline", "a\nb", "a\nb\nc"},
		{"two edits", "a\nb\nc\nd\ne\n", "A\nb\nc\nd\nE\n"},
		{"adjacent edits", "a\nb\nc\n", "A\nB\nc\n"},
		{"insert several at start", "c\n", "a\nb\nc\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := LineDiff([]byte(c.old), []byte(c.new))
			wantOld, wantNew := gitDiffRanges(t, []byte(c.old), []byte(c.new))
			if !reflect.DeepEqual(got.Old, wantOld) || !reflect.DeepEqual(got.New, wantNew) {
				t.Fatalf("LineDiff old=%v new=%v\ngit    old=%v new=%v", got.Old, got.New, wantOld, wantNew)
			}
		})
	}
}

func TestLineDiffMatchesGitRandom(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 40; i++ {
		old := randomFile(r)
		next := mutateFile(r, old)
		got := LineDiff(old, next)
		wantOld, wantNew := gitDiffRanges(t, old, next)
		if !reflect.DeepEqual(got.Old, wantOld) || !reflect.DeepEqual(got.New, wantNew) {
			t.Fatalf("case %d\nold %q\nnew %q\nLineDiff old=%v new=%v\ngit old=%v new=%v",
				i, old, next, got.Old, got.New, wantOld, wantNew)
		}
	}
}

func randomFile(r *rand.Rand) []byte {
	n := r.Intn(10)
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "line-%d-%d", i, r.Intn(4))
		if i < n-1 || r.Intn(2) == 0 {
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

func mutateFile(r *rand.Rand, src []byte) []byte {
	lines := append([]string(nil), splitLines(src)...)
	if len(lines) == 0 {
		return []byte("added\n")
	}
	switch r.Intn(3) {
	case 0:
		lines[r.Intn(len(lines))] = fmt.Sprintf("edited-%d\n", r.Intn(5))
	case 1:
		i := r.Intn(len(lines) + 1)
		ins := fmt.Sprintf("ins-%d\n", r.Intn(5))
		lines = append(lines[:i], append([]string{ins}, lines[i:]...)...)
	default:
		i := r.Intn(len(lines))
		lines = append(lines[:i], lines[i+1:]...)
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
	}
	return []byte(b.String())
}

func gitDiffRanges(t *testing.T, old, new []byte) (oldR, newR []LineRange) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/old", old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/new", new, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "diff.algorithm=myers", "diff", "--no-index", "--no-color", "-U0", "--", "old", "new")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_PAGER=cat")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			t.Fatalf("git diff: %v\n%s", err, stderr.String())
		}
	}
	return parseGitHunks(stdout.String())
}

// parseGitHunks keeps zero-length hunk sides as zero-width ranges, which
// parseUnifiedZero drops.
func parseGitHunks(diff string) (oldR, newR []LineRange) {
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		oldR = append(oldR, gitHunk(f[1]))
		newR = append(newR, gitHunk(f[2]))
	}
	return oldR, newR
}

func gitHunk(s string) LineRange {
	s = s[1:]
	start, count := s, "1"
	if i := strings.IndexByte(s, ','); i >= 0 {
		start, count = s[:i], s[i+1:]
	}
	a, _ := strconv.Atoi(start)
	n, _ := strconv.Atoi(count)
	if n == 0 {
		return LineRange{Start: a, End: a - 1}
	}
	return LineRange{Start: a, End: a + n - 1}
}
