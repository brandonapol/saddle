package bench

import (
	"io/fs"
	"os"
	"path/filepath"
)

// CopyFixture copies the fixture module at src into dst.
func CopyFixture(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
}

// WriteTo applies c to the checkout at dir.
func (c Change) WriteTo(dir string) error {
	for _, p := range c.Removes {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(p))); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for p, s := range c.Writes {
		out := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, []byte(s), 0o644); err != nil {
			return err
		}
	}
	return nil
}
