package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// UpRecord is what a running saddle up writes to .saddle/up.json, so other
// commands can tell when it runs an older binary than the one installed.
type UpRecord struct {
	PID     int       `json:"pid"`
	Version string    `json:"version"`
	Commit  string    `json:"commit"`
	Started time.Time `json:"started"`
}

const upRecordName = "up.json"

// WriteUpRecord records that saddle up (pid, built as info) is running.
// remove deletes the record if it is still this process's.
func WriteUpRecord(stateDir string, info Info, pid int, now time.Time) (remove func(), err error) {
	rec := UpRecord{PID: pid, Version: info.Version, Commit: info.Commit, Started: now.UTC()}
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(stateDir, upRecordName)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, p); err != nil {
		return nil, err
	}
	return func() {
		if cur, ok, _ := ReadUpRecord(stateDir); ok && cur.PID == pid {
			_ = os.Remove(p)
		}
	}, nil
}

// ReadUpRecord reads .saddle/up.json; ok is false when there is none.
func ReadUpRecord(stateDir string) (rec UpRecord, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(stateDir, upRecordName))
	if errors.Is(err, os.ErrNotExist) {
		return UpRecord{}, false, nil
	}
	if err != nil {
		return UpRecord{}, false, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return UpRecord{}, false, err
	}
	return rec, true, nil
}

// ProcessAlive reports whether pid is a live process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// SkewWarning returns a warning when the saddle up in rec is still running
// and older than cur (the installed binary), else "". Release versions
// compare as SemVer; dev builds compare commits.
func SkewWarning(rec UpRecord, cur Info, alive func(int) bool) string {
	if rec.PID == 0 || !alive(rec.PID) {
		return ""
	}
	upV, ok1 := ParseSemver(rec.Version)
	curV, ok2 := ParseSemver(cur.Version)
	switch {
	case ok1 && ok2:
		if Compare(upV, curV) >= 0 {
			return ""
		}
	case rec.Commit == cur.Commit && rec.Version == cur.Version:
		return ""
	}
	return fmt.Sprintf("saddle up (pid %d) is running %s (commit %s), older than the installed %s (commit %s): restart it (quit and run saddle up again) so the orchestrator and new agents use the new binary",
		rec.PID, rec.Version, short(rec.Commit), cur.Version, short(cur.Commit))
}
