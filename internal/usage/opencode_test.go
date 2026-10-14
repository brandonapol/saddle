package usage

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openCodeDB builds an opencode.db with the tables opencode 1.19.0 keeps
// sessions and messages in, filled from testdata/opencode/messages.jsonl:
// messages of ses_a (cwd /repo/wt) and ses_b (cwd /repo/other).
func openCodeDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"CREATE TABLE session (id text PRIMARY KEY, directory text NOT NULL)",
		"CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL)",
		"INSERT INTO session VALUES ('ses_a','/repo/wt'),('ses_b','/repo/other')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open("testdata/opencode/messages.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m struct {
			ID, Session      string
			Created, Updated int64
			Data             json.RawMessage
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO message VALUES (?,?,?,?,?)", m.ID, m.Session, m.Created, m.Updated, string(m.Data)); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestOpenCodeDBPath(t *testing.T) {
	if got := OpenCodeDBPath("/x/data"); got != "/x/data/opencode/opencode.db" {
		t.Fatalf("got %s", got)
	}
}

// Completed assistant messages of the worktree's sessions become records
// named provider/model; user, unfinished and other-directory messages don't.
func TestReadOpenCodeRecordsPerProviderModel(t *testing.T) {
	db := openCodeDB(t)
	recs, cur, err := ReadOpenCode(db, "/repo/wt", OpenCodeCursor{})
	if err != nil {
		t.Fatal(err)
	}
	// Errored turns carry no tokens and are not counted as messages.
	var ids []string
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	want := []string{"msg_02", "msg_03"}
	if len(ids) != len(want) {
		t.Fatalf("ids %v want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids %v want %v", ids, want)
		}
	}
	r := recs[1]
	if r.Model != "opencode/big-pickle" {
		t.Fatalf("model %q", r.Model)
	}
	// Reasoning counts as output; cache.write is cache creation.
	if r.Tokens != (Tokens{Input: 14331, Output: 120, CacheRead: 1792}) {
		t.Fatalf("tokens %+v", r.Tokens)
	}
	if !r.Time.Equal(time.UnixMilli(1791955969963)) {
		t.Fatalf("time %v", r.Time)
	}
	if cur.After != 1791956401000 {
		t.Fatalf("cursor %+v", cur)
	}

	// Polling again with the cursor returns nothing; new rows come once.
	again, cur2, err := ReadOpenCode(db, "/repo/wt", cur)
	if err != nil || len(again) != 0 {
		t.Fatalf("again %v %v", again, err)
	}
	if cur2.After != cur.After {
		t.Fatalf("cursor moved: %+v", cur2)
	}
}

func TestReadOpenCodeAllDirectories(t *testing.T) {
	recs, _, err := ReadOpenCode(openCodeDB(t), "", OpenCodeCursor{})
	if err != nil {
		t.Fatal(err)
	}
	by := OpenCodeByModel(recs)
	if len(by) != 2 {
		t.Fatalf("%+v", by)
	}
	// Sorted by model: anthropic first.
	a := by[0]
	if a.Model != "anthropic/claude-sonnet-4-5" || a.Tokens != (Tokens{Input: 1000, Output: 200, CacheRead: 200, CacheCreation: 100}) || a.CostUSD != 0.0123 || a.Messages != 1 {
		t.Fatalf("%+v", a)
	}
	if bp := by[1]; bp.Model != "opencode/big-pickle" || bp.Input != 8907+14331 || bp.Messages != 2 {
		t.Fatalf("%+v", bp)
	}
}

// A message rewritten after it was counted is not counted twice, and a
// later one is picked up.
func TestReadOpenCodeCursorDedupes(t *testing.T) {
	db := openCodeDB(t)
	_, cur, err := ReadOpenCode(db, "/repo/wt", OpenCodeCursor{})
	if err != nil {
		t.Fatal(err)
	}
	h, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	// Same updated time as the cursor: already seen.
	if _, err := h.Exec(`INSERT INTO message VALUES ('msg_09','ses_a',1791956500000,?,?)`, cur.After,
		`{"role":"assistant","tokens":{"input":5,"output":1,"cache":{}},"modelID":"m","providerID":"p","time":{"created":1,"completed":2}}`); err != nil {
		t.Fatal(err)
	}
	recs, cur, err := ReadOpenCode(db, "/repo/wt", cur)
	if err != nil || len(recs) != 1 || recs[0].ID != "msg_09" {
		t.Fatalf("%v %v", recs, err)
	}
	recs, _, err = ReadOpenCode(db, "/repo/wt", cur)
	if err != nil || len(recs) != 0 {
		t.Fatalf("recount: %v %v", recs, err)
	}
}

func TestReadOpenCodeMissingDB(t *testing.T) {
	recs, cur, err := ReadOpenCode(filepath.Join(t.TempDir(), "none.db"), "", OpenCodeCursor{})
	if err != nil || len(recs) != 0 || cur.After != 0 {
		t.Fatalf("%v %+v %v", recs, cur, err)
	}
}

// Unreadable data (a file that isn't a database) is an error, not a panic
// or a silent zero.
func TestReadOpenCodeGarbageDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(p, []byte("not sqlite at all, just text padding padding padding padding"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadOpenCode(p, "", OpenCodeCursor{}); err == nil {
		t.Fatal("want error")
	}
}

func TestReadOpenCodeLimits(t *testing.T) {
	ls, err := ReadOpenCodeLimits(openCodeDB(t), "/repo/wt", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 {
		t.Fatalf("%+v", ls)
	}
	if l := ls[0]; l.Provider != "openai" || l.ModelID != "gpt-5" || l.Model() != "openai/gpt-5" || l.Resets != "" || l.ResetIn != 0 {
		t.Fatalf("%+v", l)
	}
	l := ls[1]
	if l.Model() != "opencode/big-pickle" || l.Resets != "in 2 hours 5 minutes" || l.ResetIn != 2*time.Hour+5*time.Minute {
		t.Fatalf("%+v", l)
	}
	if !l.Time.Equal(time.UnixMilli(1791956301000)) {
		t.Fatalf("time %v", l.Time)
	}
	// Only errors newer than the given time.
	ls, _ = ReadOpenCodeLimits(openCodeDB(t), "/repo/wt", 1791956250000)
	if len(ls) != 1 || ls[0].ModelID != "big-pickle" {
		t.Fatalf("%+v", ls)
	}
}

func TestParseOpenCodeLimit(t *testing.T) {
	cases := []struct {
		in      string
		ok      bool
		resets  string
		resetIn time.Duration
	}{
		{"Free usage exceeded, subscribe to Go", true, "", 0},
		{"Go usage limit reached. It will reset in 2 hours 5 minutes. To continue", true, "in 2 hours 5 minutes", 125 * time.Minute},
		{"Quota exceeded. Check your plan and billing details.", true, "", 0},
		{"You exceeded your current quota, please check your plan", true, "", 0},
		{"code: insufficient_quota", true, "", 0},
		{"usage limit reached. It will reset in 1 day 3 hours.", true, "in 1 day 3 hours", 27 * time.Hour},
		{"Bad request: invalid tool schema", false, "", 0},
		{"", false, "", 0},
	}
	for _, c := range cases {
		l, ok := ParseOpenCodeLimit(c.in)
		if ok != c.ok || l.Resets != c.resets || l.ResetIn != c.resetIn {
			t.Errorf("%q: %+v %v", c.in, l, ok)
		}
	}
}
