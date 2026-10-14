package agent

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var ocNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func ocScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "opencode", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// #531: the quota pane marks the failing model out until the reset the
// banner words as a length of time.
func TestOpenCodeScreenLimit(t *testing.T) {
	out, ok := OpenCodeScreenLimit(ocScreen(t, "quota.txt"), "openai/gpt-5", ocNow)
	if !ok {
		t.Fatal("quota pane not detected")
	}
	if out.Adapter != "opencode" || out.Model != "openai/gpt-5" {
		t.Fatalf("%+v", out)
	}
	if want := ocNow.Add(2*time.Hour + 5*time.Minute); !out.Until.Equal(want) {
		t.Fatalf("until %v want %v", out.Until, want)
	}
	if out.Resets != "" && out.Resets != "in 2 hours 5 minutes" {
		t.Fatalf("resets %q", out.Resets)
	}
}

// With no model known the whole adapter is out; with no reset in the text
// it is out for the default hour.
func TestOpenCodeScreenLimitWholeAdapterDefaultReset(t *testing.T) {
	out, ok := OpenCodeScreenLimit("  hello\n\n     Free usage exceeded, subscribe to Go\n\n  ┃\n", "", ocNow)
	if !ok || out.Model != "" || !out.Until.Equal(ocNow.Add(OpenCodeExhaustFor)) {
		t.Fatalf("%+v %v", out, ok)
	}
}

func TestOpenCodeScreenLimitNegative(t *testing.T) {
	for _, n := range []string{"idle.txt", "busy.txt", "approval.txt", "worker-idle.txt", "draft.txt"} {
		if _, ok := OpenCodeScreenLimit(ocScreen(t, n), "m", ocNow); ok {
			t.Errorf("%s detected as a limit", n)
		}
	}
	// The same words far up in scrollback are not a banner.
	scroll := "Quota exceeded. Check your plan.\n" + strings.Repeat("work line\n", 30)
	if _, ok := OpenCodeScreenLimit(scroll, "m", ocNow); ok {
		t.Error("scrollback detected")
	}
}

func ocDB(t *testing.T, rows ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	qs := []string{
		"CREATE TABLE session (id text PRIMARY KEY, directory text NOT NULL)",
		"CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL)",
		"INSERT INTO session VALUES ('s','/repo/wt')",
	}
	for _, q := range append(qs, rows...) {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// The session database names the provider/model that failed; the reset is
// counted from when the turn failed, and the newest limit wins.
func TestOpenCodeDBLimit(t *testing.T) {
	failed := ocNow.Add(-30 * time.Minute).UnixMilli()
	db := ocDB(t,
		`INSERT INTO message VALUES ('m1','s',1,1,'{"role":"assistant","providerID":"openai","modelID":"gpt-5","time":{"completed":1},"error":{"data":{"message":"Quota exceeded. Check your plan and billing details."}}}')`,
		`INSERT INTO message VALUES ('m2','s',2,`+itoa(failed)+`,'{"role":"assistant","providerID":"opencode","modelID":"big-pickle","time":{"completed":2},"error":{"data":{"message":"Go usage limit reached. It will reset in 2 hours 5 minutes."}}}')`,
	)
	out, ok, err := OpenCodeDBLimit(db, "/repo/wt", 0, ocNow)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if out.Model != "opencode/big-pickle" {
		t.Fatalf("%+v", out)
	}
	if want := ocNow.Add(-30*time.Minute + 2*time.Hour + 5*time.Minute); !out.Until.Equal(want) {
		t.Fatalf("until %v want %v", out.Until, want)
	}
}

// A limit whose reset has already passed marks nothing out.
func TestOpenCodeDBLimitExpired(t *testing.T) {
	old := ocNow.Add(-3 * time.Hour).UnixMilli()
	db := ocDB(t, `INSERT INTO message VALUES ('m','s',1,`+itoa(old)+`,'{"role":"assistant","providerID":"opencode","modelID":"big-pickle","time":{"completed":1},"error":{"data":{"message":"Go usage limit reached. It will reset in 2 hours."}}}')`)
	if _, ok, err := OpenCodeDBLimit(db, "/repo/wt", 0, ocNow); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
}

func TestOpenCodeDBLimitNone(t *testing.T) {
	if _, ok, err := OpenCodeDBLimit(filepath.Join(t.TempDir(), "x.db"), "", 0, ocNow); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
