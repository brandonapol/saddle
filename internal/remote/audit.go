package remote

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const auditFile = "audit.log"

const (
	DecisionAllowed = "allowed"
	DecisionDenied  = "denied"
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
type Audit struct {
	mu     sync.Mutex
	f      *os.File
	Mirror func(AuditEntry)
}

// OpenAudit opens dir/audit.log for appending, creating dir 0700.
func OpenAudit(dir string) (*Audit, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, auditFile)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Audit{f: f}, nil
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if a.Mirror != nil {
		a.Mirror(e)
	}
	return nil
}

func (a *Audit) Close() error { return a.f.Close() }
