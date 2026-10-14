package sre

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// IncidentFile is the guard's state, relative to the repo root: the current
// (or last) incident. It lives with saddle's other state, not in git.
const IncidentFile = ".saddle/sre-incident.json"

// LoadIncident reads the incident under root: nil with no error when there
// has never been one. A file that does not parse is an error, so callers
// that guard production can refuse rather than guess.
func LoadIncident(root string) (*Incident, error) {
	b, err := os.ReadFile(filepath.Join(root, IncidentFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var inc Incident
	if err := json.Unmarshal(b, &inc); err != nil {
		return nil, fmt.Errorf("%s: %w", IncidentFile, err)
	}
	return &inc, nil
}

// OpenIncident starts inc as the current incident. It fails while another
// incident is open.
func OpenIncident(root string, inc *Incident) error {
	return withIncident(root, func(cur **Incident) error {
		if (*cur).Open() {
			return fmt.Errorf("incident %s is still open; close it first", (*cur).ID)
		}
		*cur = inc
		return nil
	})
}

// UpdateIncident runs fn on the open incident under a file lock and saves it
// if fn returns nil.
func UpdateIncident(root string, fn func(*Incident) error) error {
	return withIncident(root, func(cur **Incident) error {
		if !(*cur).Open() {
			return errors.New("no open SRE incident")
		}
		return fn(*cur)
	})
}

func withIncident(root string, fn func(**Incident) error) error {
	p := filepath.Join(root, IncidentFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	inc, err := LoadIncident(root)
	if err != nil {
		return err
	}
	if err := fn(&inc); err != nil {
		return err
	}
	b, err := json.MarshalIndent(inc, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
