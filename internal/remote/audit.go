package remote

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const auditFile = "audit.log"

// Audit rotation defaults: audit.log rolls to audit.log.1 past 10 MiB, and
// five old logs are kept.
const (
	DefaultAuditMaxBytes int64 = 10 << 20
	DefaultAuditKeep           = 5
)

const (
	DecisionAllowed = "allowed"
	DecisionDenied  = "denied"
	// DecisionPending is an admin call held for confirmation; it hasn't run.
	DecisionPending = "pending"
)

// AuditEntry is one line of the audit log: every failed login and every
// tool call, allowed or not.
type AuditEntry struct {
	TS       time.Time `json:"ts"`
	Token    string    `json:"token,omitempty"` // the token's name, never its secret
	Addr     string    `json:"addr"`
	Tool     string    `json:"tool,omitempty"`
	Decision string    `json:"decision"`
	Reason   string    `json:"reason,omitempty"`
}

// Audit appends entries as JSON lines to a 0600 file. Mirror, when set,
// also gets each entry (the CLI copies them into the repo's event log so
// the TUI shows who did what).
//
// Past MaxBytes the log is rotated: audit.log.N-1 moves to audit.log.N and
// audit.log to audit.log.1, keeping Keep old files. Several processes may
// share the log (an engine-run serve and ssh stdio sessions), so before
// each write Audit checks that audit.log is still the file it has open and
// reopens it if another process rotated it.
type Audit struct {
	mu       sync.Mutex
	dir      string
	f        *os.File
	size     int64
	MaxBytes int64
	Keep     int
	Mirror   func(AuditEntry)
}

// OpenAudit opens dir/audit.log for appending, creating dir 0700.
func OpenAudit(dir string) (*Audit, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	a := &Audit{dir: dir, MaxBytes: DefaultAuditMaxBytes, Keep: DefaultAuditKeep}
	if err := a.open(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Audit) path() string { return filepath.Join(a.dir, auditFile) }

func (a *Audit) open() error {
	f, err := os.OpenFile(a.path(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err != nil {
		_ = f.Close()
		return err
	}
	a.f, a.size = f, fi.Size()
	return nil
}

// current reopens audit.log when another process has rotated it away.
func (a *Audit) current() error {
	have, err := a.f.Stat()
	if err != nil {
		return err
	}
	on, err := os.Stat(a.path())
	if err == nil && os.SameFile(have, on) {
		a.size = on.Size()
		return nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = a.f.Close()
	return a.open()
}

// rotate shifts the old logs up one, drops the oldest past Keep, and starts
// a fresh audit.log.
func (a *Audit) rotate() error {
	keep := max(a.Keep, 1)
	p := a.path()
	_ = os.Remove(fmt.Sprintf("%s.%d", p, keep))
	for i := keep - 1; i >= 1; i-- {
		if err := os.Rename(fmt.Sprintf("%s.%d", p, i), fmt.Sprintf("%s.%d", p, i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(p, p+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = a.f.Close()
	return a.open()
}

// Log records e. A failed write is returned so the caller can refuse the
// call: an action that can't be audited doesn't run.
func (a *Audit) Log(e AuditEntry) error {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.current(); err != nil {
		return err
	}
	if a.MaxBytes > 0 && a.size > 0 && a.size+int64(len(b)) > a.MaxBytes {
		if err := a.rotate(); err != nil {
			return err
		}
	}
	n, err := a.f.Write(b)
	a.size += int64(n)
	if err != nil {
		return err
	}
	if a.Mirror != nil {
		a.Mirror(e)
	}
	return nil
}

func (a *Audit) Close() error { return a.f.Close() }

// ReadAudit returns every entry in dir's audit log and its rotated files,
// oldest first. Lines that don't parse are skipped.
func ReadAudit(dir string) ([]AuditEntry, error) {
	base := filepath.Join(dir, auditFile)
	olds, err := filepath.Glob(base + ".*")
	if err != nil {
		return nil, err
	}
	nums := map[string]int{}
	for _, p := range olds {
		var n int
		if _, err := fmt.Sscanf(p[len(base)+1:], "%d", &n); err == nil && fmt.Sprint(n) == p[len(base)+1:] {
			nums[p] = n
		}
	}
	var files []string
	for p := range nums {
		files = append(files, p)
	}
	slices.SortFunc(files, func(x, y string) int { return nums[y] - nums[x] }) // highest number is oldest
	files = append(files, base)
	var out []AuditEntry
	for _, p := range files {
		es, err := readAuditFile(p)
		if err != nil {
			return nil, err
		}
		out = append(out, es...)
	}
	return out, nil
}

func readAuditFile(p string) ([]AuditEntry, error) {
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
