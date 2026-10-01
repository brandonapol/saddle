package usage

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
)

// Cursor tracks how far a transcript has been consumed.
type Cursor struct {
	// Offset is the byte offset of the first unconsumed line.
	Offset int64
	// Malformed counts lines skipped because they could not be parsed.
	Malformed int64
	seen      map[string]struct{}
}

// Tail reads the complete lines of path that lie beyond c.Offset, parses them
// and advances the cursor. A trailing line without a newline is assumed to be
// still in progress and is left for the next call. If the file is shorter than
// the cursor it was truncated or replaced, and reading restarts at zero. A
// missing file yields no records and no error.
func Tail(path string, c *Cursor, parse Parser) ([]Record, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() < c.Offset {
		c.Offset = 0
		c.seen = nil
	}
	if st.Size() == c.Offset {
		return nil, nil
	}
	if _, err := f.Seek(c.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, nil
	}
	data = data[:end+1]
	c.Offset += int64(len(data))

	if c.seen == nil {
		c.seen = map[string]struct{}{}
	}
	var out []Record
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		rec, ok, err := parse(line)
		if err != nil {
			c.Malformed++
			continue
		}
		if !ok {
			continue
		}
		if rec.ID != "" {
			if _, dup := c.seen[rec.ID]; dup {
				continue
			}
			c.seen[rec.ID] = struct{}{}
		}
		out = append(out, rec)
	}
	return out, nil
}
