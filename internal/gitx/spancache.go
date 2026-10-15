package gitx

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	spanCacheMaxBytes  = 64 << 20
	spanCacheKeepBytes = 48 << 20
)

// Span is one blob's extracted symbols and parse-error ranges.
type Span struct {
	Symbols []Symbol    `json:"symbols"`
	Errors  []LineRange `json:"errors"`
}

// SpanCache stores spans at <git common dir>/saddle-symbols/v<N>/<lang>/<blob>.json.
// N is SpanCacheVersion. A write that finds the directory over 64 MiB deletes
// the least recently modified entries until it is under 48 MiB.
type SpanCache struct {
	root      string
	version   int
	maxBytes  int64
	keepBytes int64
}

// OpenSpanCache returns the cache rooted at gitCommonDir, the path
// `git rev-parse --git-common-dir` prints.
func OpenSpanCache(gitCommonDir string) *SpanCache {
	return &SpanCache{
		root:      filepath.Join(gitCommonDir, "saddle-symbols"),
		version:   SpanCacheVersion,
		maxBytes:  spanCacheMaxBytes,
		keepBytes: spanCacheKeepBytes,
	}
}

// Get returns the span for lang and blob, calling fill only on a miss.
// The write is a temp file renamed into place, so a reader never sees a
// partial entry. lang and blob must be a single safe path element.
func (c *SpanCache) Get(lang, blob string, fill func() (Span, error)) (Span, error) {
	if !cacheKeyOK(lang) || !cacheKeyOK(blob) {
		return Span{}, fmt.Errorf("span cache: bad key %q %q", lang, blob)
	}
	if fill == nil {
		return Span{}, fmt.Errorf("span cache: nil fill")
	}
	path := c.entryPath(lang, blob)
	if s, ok := readSpan(path); ok {
		return s, nil
	}
	s, err := fill()
	if err != nil {
		return Span{}, err
	}
	data, err := marshalSpan(s)
	if err != nil {
		return Span{}, err
	}
	if err := writeAtomic(path, data); err != nil {
		return Span{}, err
	}
	if err := c.prune(); err != nil {
		return Span{}, err
	}
	return s, nil
}

func (c *SpanCache) entryPath(lang, blob string) string {
	return filepath.Join(c.root, fmt.Sprintf("v%d", c.version), lang, blob+".json")
}

func cacheKeyOK(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func marshalSpan(s Span) ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func readSpan(path string) (Span, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Span{}, false
	}
	var s Span
	if json.Unmarshal(b, &s) != nil {
		return Span{}, false
	}
	return s, true
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	keep = true
	return nil
}

type cacheFile struct {
	path string
	size int64
	mod  int64
}

func (c *SpanCache) prune() error {
	files, total, err := c.cacheFiles()
	if err != nil || total <= c.maxBytes {
		return err
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].mod != files[j].mod {
			return files[i].mod < files[j].mod
		}
		return files[i].path < files[j].path
	})
	for _, f := range files {
		if total <= c.keepBytes {
			break
		}
		if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		total -= f.size
	}
	return nil
}

func (c *SpanCache) cacheFiles() ([]cacheFile, int64, error) {
	var files []cacheFile
	var total int64
	err := filepath.WalkDir(c.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		files = append(files, cacheFile{path: path, size: info.Size(), mod: info.ModTime().UnixNano()})
		total += info.Size()
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, 0, err
	}
	return files, total, nil
}

// BlobID is the git blob id of content for the repo's object format,
// "sha1" or "sha256": sha of "blob <len>\0" + content.
func BlobID(content []byte, format string) (string, error) {
	var h hash.Hash
	switch format {
	case "sha1":
		h = sha1.New()
	case "sha256":
		h = sha256.New()
	default:
		return "", fmt.Errorf("blob id: unknown object format %q", format)
	}
	if _, err := fmt.Fprintf(h, "blob %d\x00", len(content)); err != nil {
		return "", err
	}
	if _, err := h.Write(content); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ObjectFormat is the repo's object format, "sha1" or "sha256".
func ObjectFormat(dir string) (string, error) {
	s, err := Run(dir, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	switch s {
	case "sha1", "sha256":
		return s, nil
	default:
		return "", fmt.Errorf("object format %q", s)
	}
}
