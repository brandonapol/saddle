package gitx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSpanCacheSecondLookupSkipsExtractor(t *testing.T) {
	c := OpenSpanCache(t.TempDir())
	blob := strings.Repeat("ab", 20)
	want := Span{
		Symbols: []Symbol{{Name: "A", Kind: "func", StartLine: 2, EndLine: 4, SigHash: "abc"}},
		Errors:  []LineRange{{Start: 9, End: 9}},
	}
	n := 0
	fill := func() (Span, error) {
		n++
		return want, nil
	}
	got, err := c.Get("go", blob, fill)
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.Get("go", blob, fill)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("fill calls = %d, want 1", n)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(again, want) {
		t.Fatalf("got %+v again %+v want %+v", got, again, want)
	}
	if _, err := os.Stat(c.entryPath("go", blob)); err != nil {
		t.Fatal(err)
	}
}

func TestSpanCacheVersionMiss(t *testing.T) {
	c := OpenSpanCache(t.TempDir())
	blob := strings.Repeat("cd", 20)
	n := 0
	fill := func() (Span, error) {
		n++
		return Span{Symbols: []Symbol{{Name: "A", Kind: "func"}}}, nil
	}
	if _, err := c.Get("python", blob, fill); err != nil {
		t.Fatal(err)
	}
	c.version++
	if _, err := c.Get("python", blob, fill); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("fill calls = %d, want 2 after a version bump", n)
	}
	if _, err := os.Stat(filepath.Join(c.root, fmt.Sprintf("v%d", SpanCacheVersion), "python", blob+".json")); err != nil {
		t.Fatal(err)
	}
}

func TestSpanCachePruneOldestFirst(t *testing.T) {
	c := OpenSpanCache(t.TempDir())
	var paths []string
	for i := 0; i < 3; i++ {
		blob := fmt.Sprintf("%040d", i)
		if _, err := c.Get("go", blob, func() (Span, error) {
			return Span{Symbols: []Symbol{{Name: fmt.Sprintf("n%d", i), Kind: "func"}}}, nil
		}); err != nil {
			t.Fatal(err)
		}
		p := c.entryPath("go", blob)
		paths = append(paths, p)
		ts := time.Unix(1_700_000_000+int64(i)*100, 0)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	var total int64
	sizes := make([]int64, len(paths))
	for i, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		sizes[i] = fi.Size()
		total += fi.Size()
	}
	next := Span{Symbols: []Symbol{{Name: "new", Kind: "func"}}}
	raw, err := marshalSpan(next)
	if err != nil {
		t.Fatal(err)
	}
	// One byte over the cap, and room after the write for every entry but the oldest.
	c.maxBytes = total
	c.keepBytes = total + int64(len(raw)) - sizes[0]
	blob := fmt.Sprintf("%040d", 3)
	if _, err := c.Get("go", blob, func() (Span, error) { return next, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[0]); !os.IsNotExist(err) {
		t.Fatalf("oldest entry still present: %v", err)
	}
	for _, p := range paths[1:] {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("newer entry %s: %v", p, err)
		}
	}
	if _, err := os.Stat(c.entryPath("go", blob)); err != nil {
		t.Fatal(err)
	}
}

func TestSpanCacheConcurrentWriters(t *testing.T) {
	c := OpenSpanCache(t.TempDir())
	blob := strings.Repeat("ef", 20)
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errc := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := c.Get("go", blob, func() (Span, error) {
				return Span{Symbols: []Symbol{{Name: "A", Kind: "func", StartLine: 1, EndLine: 3}}}, nil
			})
			errc <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(c.entryPath("go", blob))
	if err != nil {
		t.Fatal(err)
	}
	var s Span
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("entry not readable: %v\n%s", err, b)
	}
	if len(s.Symbols) != 1 || s.Symbols[0].Name != "A" {
		t.Fatalf("entry = %+v", s)
	}
}

func TestBlobIDMatchesGit(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	bodies := [][]byte{nil, []byte("a"), []byte("a\n"), []byte("hello\nworld"), []byte("no-nl")}
	for _, format := range []string{"sha1", "sha256"} {
		dir := t.TempDir()
		cmd := exec.Command("git", "init", "-q", "--object-format="+format, dir)
		cmd.Env = os.Environ()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init %s: %v\n%s", format, err, out)
		}
		got, err := ObjectFormat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got != format {
			t.Fatalf("ObjectFormat = %q, want %q", got, format)
		}
		for _, body := range bodies {
			id, err := BlobID(body, format)
			if err != nil {
				t.Fatal(err)
			}
			hash := gitHashObject(t, dir, body)
			if id != hash {
				t.Fatalf("%s %q: BlobID %s, git %s", format, body, id, hash)
			}
		}
	}
}

func gitHashObject(t *testing.T, dir string, body []byte) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "hash-object", "--stdin")
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}
