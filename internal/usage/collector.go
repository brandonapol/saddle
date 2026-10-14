package usage

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session is one agent session whose transcript is being tailed.
type Session struct {
	ID       string // agent session id, as given by hooks
	Task     string // saddle task the usage is attributed to
	Agent    string // Claude, Codex or Grok
	Path     string // transcript file; opencode's session database
	Dir      string // opencode: only sessions run in this directory count
	Model    string // configured model when the transcript omits it
	Snapshot bool   // Path is a rewritten usage snapshot, not append-only JSONL
}

type tracked struct {
	Session
	cur      Cursor
	ocCur    OpenCodeCursor // opencode's place in its database
	parse    Parser
	snapshot SnapshotParser
	// rollout caches the model read from a Codex rollout at rolloutPath.
	rollout, rolloutPath string
}

// Collector tails transcripts for registered sessions and aggregates the new
// usage into minute buckets. It is not safe for concurrent use.
type Collector struct {
	// Now supplies the timestamp for records that carry none.
	Now      func() time.Time
	sessions map[string]*tracked
}

// NewCollector returns an empty Collector.
func NewCollector() *Collector {
	return &Collector{Now: time.Now, sessions: map[string]*tracked{}}
}

// Track registers (or re-registers, keeping its cursor) a session. It reports
// false if the agent kind is unknown.
func (c *Collector) Track(s Session) bool {
	p := ParserFor(s.Agent)
	if s.Agent == OpenCode {
		p = func([]byte) (Record, bool, error) { return Record{}, false, nil } // read from the database, not tailed
	}
	var snap SnapshotParser
	if s.Snapshot && s.Agent == Grok {
		snap = ParseGrokSnapshot
	}
	if p == nil || (s.Snapshot && snap == nil) {
		return false
	}
	if s.Agent == Codex {
		p = NewCodexParser()
	}
	if t, ok := c.sessions[s.ID]; ok {
		// A Codex parser carries the current turn's model: keep it with
		// the cursor.
		if t.Agent == Codex && s.Agent == Codex && t.Path == s.Path {
			p = t.parse
		}
		t.Session, t.parse, t.snapshot = s, p, snap
		return true
	}
	c.sessions[s.ID] = &tracked{Session: s, parse: p, snapshot: snap}
	return true
}

// Untrack stops tailing a session.
func (c *Collector) Untrack(id string) { delete(c.sessions, id) }

// Cursor returns a copy of a session's cursor.
func (c *Collector) Cursor(id string) (Cursor, bool) {
	t, ok := c.sessions[id]
	if !ok {
		return Cursor{}, false
	}
	cur := t.cur
	if t.Agent == OpenCode {
		cur.Offset = t.ocCur.After // nonzero once a turn has been read
	}
	return cur, true
}

// Poll reads new transcript lines for every session and returns the usage
// since the previous Poll as buckets. Each token is reported exactly once.
// The first error encountered is returned alongside whatever was collected.
func (c *Collector) Poll() ([]Bucket, error) {
	agg := NewAggregator()
	var firstErr error
	for _, t := range c.sessions {
		var recs []Record
		var err error
		if t.Agent == OpenCode {
			recs, t.ocCur, err = ReadOpenCode(t.Path, t.Dir, t.ocCur)
		} else if t.snapshot != nil {
			recs, err = ReadSnapshot(t.Path, &t.cur, t.snapshot)
		} else {
			recs, err = Tail(t.Path, &t.cur, t.parse)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
		model := t.Model
		if model == "" && t.Agent == Codex && len(recs) > 0 {
			if t.rolloutPath != t.Path || t.rollout == "" {
				t.rollout, _ = CodexRolloutModel(t.Path)
				t.rolloutPath = t.Path
			}
			model = t.rollout
		}
		for _, r := range recs {
			if r.Time.IsZero() {
				r.Time = c.Now()
			}
			if model != "" && (r.Model == "" || r.Model == t.Agent) {
				r.Model = model
			}
			agg.Add(t.Task, t.Agent, r)
		}
	}
	return agg.Flush(), firstErr
}

// ClaudeTranscriptPath is where Claude Code stores a session transcript:
// <configDir>/projects/<cwd with / and . replaced by ->/<session>.jsonl.
// An empty configDir means ~/.claude.
func ClaudeTranscriptPath(configDir, cwd, sessionID string) string {
	if configDir == "" {
		home, _ := os.UserHomeDir()
		configDir = filepath.Join(home, ".claude")
	}
	enc := strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
	return filepath.Join(configDir, "projects", enc, sessionID+".jsonl")
}

// FindClaudeTranscript locates a session transcript by id under any project
// directory, for when the cwd encoding is not known. It returns "" if absent.
func FindClaudeTranscript(configDir, sessionID string) string {
	if configDir == "" {
		home, _ := os.UserHomeDir()
		configDir = filepath.Join(home, ".claude")
	}
	m, _ := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}
