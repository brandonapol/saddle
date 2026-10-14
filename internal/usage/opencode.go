package usage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// OpenCode is the agent name opencode usage is attributed to.
const OpenCode = "opencode"

// OpenCodeDBPath is opencode 1.19.0's session database under dataHome
// ($XDG_DATA_HOME, default ~/.local/share): <dataHome>/opencode/opencode.db.
// An empty dataHome means the default.
func OpenCodeDBPath(dataHome string) string {
	if dataHome == "" {
		home, _ := os.UserHomeDir()
		dataHome = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataHome, "opencode", "opencode.db")
}

// OpenCodeCursor remembers how far ReadOpenCode has read. The zero value
// reads everything.
type OpenCodeCursor struct {
	After int64    `json:"after"`          // newest message time_updated seen, in ms
	Seen  []string `json:"seen,omitempty"` // ids of the messages updated at exactly After
}

// openCodeMessage is the part of a message row's JSON that usage reads. An
// assistant message carries the provider and model it ran on, its token
// counts and cost, a completed time once the turn ends, and an error when
// the provider refused it.
type openCodeMessage struct {
	Role       string `json:"role"`
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
	Cost       float64
	Tokens     struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Time struct {
		Completed int64 `json:"completed"`
	} `json:"time"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message         string            `json:"message"`
			StatusCode      int               `json:"statusCode"`
			ResponseHeaders map[string]string `json:"responseHeaders"`
			ResponseBody    string            `json:"responseBody"`
		} `json:"data"`
	} `json:"error"`
}

func (m openCodeMessage) model() string {
	if m.ProviderID == "" {
		return m.ModelID
	}
	return m.ProviderID + "/" + m.ModelID
}

// openOpenCode opens the database read-only. A missing file is not an
// error: opencode has not run yet.
func openOpenCode(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
}

// openCodeRows calls fn for each message of the sessions run in dir (all
// sessions when dir is empty) updated at or after ms, oldest first.
func openCodeRows(path, dir string, ms int64, fn func(id string, updated int64, m openCodeMessage)) error {
	db, err := openOpenCode(path)
	if err != nil || db == nil {
		return err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT m.id, m.time_updated, m.data FROM message m JOIN session s ON s.id = m.session_id
		WHERE (? = '' OR s.directory = ?) AND m.time_updated >= ? ORDER BY m.time_updated, m.id`, dir, dir, ms)
	if err != nil {
		return fmt.Errorf("opencode db: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, data string
		var updated int64
		if err := rows.Scan(&id, &updated, &data); err != nil {
			return fmt.Errorf("opencode db: %w", err)
		}
		var m openCodeMessage
		if json.Unmarshal([]byte(data), &m) != nil {
			continue // a row in a shape this reader doesn't know
		}
		fn(id, updated, m)
	}
	return rows.Err()
}

// ReadOpenCode returns the usage of opencode turns that completed since
// cur, for the sessions run in dir (all sessions when dir is empty). Each
// record is one assistant message, named provider/model, with reasoning
// counted as output and cache writes as cache creation. Turns that
// errored before producing tokens are left out. It returns the cursor to
// pass next time; a missing database is no usage, not an error.
func ReadOpenCode(dbPath, dir string, cur OpenCodeCursor) ([]Record, OpenCodeCursor, error) {
	next := OpenCodeCursor{After: cur.After, Seen: slices.Clone(cur.Seen)}
	var recs []Record
	err := openCodeRows(dbPath, dir, cur.After, func(id string, updated int64, m openCodeMessage) {
		if updated == cur.After && slices.Contains(cur.Seen, id) {
			return
		}
		switch {
		case updated > next.After:
			next.After, next.Seen = updated, []string{id}
		case updated == next.After:
			next.Seen = append(next.Seen, id)
		}
		if m.Role != "assistant" || m.Time.Completed == 0 {
			return
		}
		t := Tokens{
			Input:         m.Tokens.Input,
			Output:        m.Tokens.Output + m.Tokens.Reasoning,
			CacheRead:     m.Tokens.Cache.Read,
			CacheCreation: m.Tokens.Cache.Write,
		}
		if t.Total() == 0 && m.Cost == 0 {
			return
		}
		recs = append(recs, Record{ID: id, Time: time.UnixMilli(m.Time.Completed), Model: m.model(), Tokens: t, CostUSD: m.Cost})
	})
	return recs, next, err
}

// OpenCodeUsage is the usage of one provider/model.
type OpenCodeUsage struct {
	Model string `json:"model"` // provider/model
	Tokens
	Messages int64   `json:"messages"`
	CostUSD  float64 `json:"cost_usd"`
}

// OpenCodeByModel sums records per provider/model, sorted by model.
func OpenCodeByModel(recs []Record) []OpenCodeUsage {
	m := map[string]*OpenCodeUsage{}
	for _, r := range recs {
		u := m[r.Model]
		if u == nil {
			u = &OpenCodeUsage{Model: r.Model}
			m[r.Model] = u
		}
		u.Tokens = u.Add(r.Tokens)
		u.Messages++
		u.CostUSD += r.CostUSD
	}
	out := make([]OpenCodeUsage, 0, len(m))
	for _, u := range m {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// OpenCodeLimit is a provider refusing opencode for quota or plan limits.
type OpenCodeLimit struct {
	Provider string        `json:"provider,omitempty"` // empty when not known (a screen)
	ModelID  string        `json:"model_id,omitempty"`
	Message  string        `json:"message"`
	Resets   string        `json:"resets,omitempty"`   // "in 2 hours 5 minutes"; empty when the error names none
	ResetIn  time.Duration `json:"reset_in,omitempty"` // Resets as a length of time
	// RetryIn is how long opencode waits before retrying, from its status
	// line ("[retrying in 9h 29m attempt #1]"). opencode waits out a
	// provider's retry-after, so for Zen's limits it is the reset; for other
	// providers it is a backoff of seconds.
	RetryIn time.Duration `json:"retry_in,omitempty"`
	Time    time.Time     `json:"time,omitzero"` // when the failing turn ended
}

// Model is provider/model, or "" when the error does not say.
func (l OpenCodeLimit) Model() string {
	if l.ModelID == "" {
		return ""
	}
	if l.Provider == "" {
		return l.ModelID
	}
	return l.Provider + "/" + l.ModelID
}

var (
	// The wordings in opencode 1.19.0 and OpenCode Zen: opencode's own free
	// tier status and dialog, Go's "5 hour usage limit reached" (opencode)
	// and "5-hour usage limit reached" (Zen), Zen's subscription quota,
	// balance and spending limits, and a provider's quota or billing error.
	openCodeLimitRe = regexp.MustCompile(`(?i)free usage exceeded|free limit reached|usage limit reached|\bquota exceeded|exceeded your current quota|insufficient_quota|insufficient balance|monthly spending limit|credit balance is too low`)
	// "It will reset in 2 hours 5 minutes" (opencode), "Resets in 2hr
	// 5min" (Zen Go), "Retry in 45min" (Zen subscription).
	openCodeResetRe = regexp.MustCompile(`(?i)\b(?:will reset|resets|retry) (in (?:~?\d+\s*` + openCodeUnit + `\s*(?:and\s+)?)+)`)
	// opencode's retry status: " [retrying in 9h 29m attempt #1]".
	openCodeRetryRe = regexp.MustCompile(`(?i)\[retrying (in (?:~?\d+\s*` + openCodeUnit + `\s*)+)attempt #\d+\]`)
	openCodePartRe  = regexp.MustCompile(`(?i)(\d+)\s*` + openCodeUnit)
)

// openCodeUnit is a unit of time as opencode and Zen write it, long or
// short: "2 hours", "2hr", "2h", "~2 days", "1 week".
const openCodeUnit = `(weeks?|w|days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\b`

var openCodeUnits = map[byte]time.Duration{'w': 7 * 24 * time.Hour, 'd': 24 * time.Hour, 'h': time.Hour, 'm': time.Minute, 's': time.Second}

func openCodeDuration(s string) time.Duration {
	var d time.Duration
	for _, p := range openCodePartRe.FindAllStringSubmatch(s, -1) {
		n, _ := strconv.Atoi(p[1])
		d += time.Duration(n) * openCodeUnits[strings.ToLower(p[2])[0]]
	}
	return d
}

// ParseOpenCodeLimit reports whether text (an error message or screen
// text) is one of opencode's limit wordings, when it resets, and how long
// opencode's status line says it waits before retrying.
func ParseOpenCodeLimit(text string) (OpenCodeLimit, bool) {
	// opencode's spinner draws over the status line's leading spaces with
	// ⬝, and a banner wraps mid-sentence.
	text = strings.Join(strings.Fields(strings.ReplaceAll(text, "⬝", " ")), " ")
	if !openCodeLimitRe.MatchString(text) {
		return OpenCodeLimit{}, false
	}
	l := OpenCodeLimit{Message: text}
	if m := openCodeResetRe.FindStringSubmatch(text); m != nil {
		l.Resets = strings.TrimRight(strings.TrimSpace(m[1]), ".,")
		l.ResetIn = openCodeDuration(l.Resets)
	}
	if m := openCodeRetryRe.FindStringSubmatch(text); m != nil {
		l.RetryIn = openCodeDuration(m[1])
	}
	return l, true
}

// openCodeQuotaTypes are the error types and codes in a provider's
// response body that mean quota, not a passing rate limit: OpenCode Zen's
// (its free tier's message is a bare "Rate limit exceeded. Please try
// again later.") and OpenAI's. opencode keys its own free tier and Go
// notices on the same types.
var openCodeQuotaTypes = []string{
	"FreeUsageLimitError", "GoUsageLimitError", "BlackUsageLimitError",
	"CreditsError", "MonthlyLimitError", "UserLimitError",
	"insufficient_quota",
}

// openCodeQuotaBody reports whether a provider's response body names one of
// openCodeQuotaTypes as its error's type or code.
func openCodeQuotaBody(body string) bool {
	var b struct {
		Error struct {
			Type string `json:"type"`
			Code any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &b) != nil {
		return false
	}
	code, _ := b.Error.Code.(string)
	return slices.Contains(openCodeQuotaTypes, b.Error.Type) || slices.Contains(openCodeQuotaTypes, code)
}

// ReadOpenCodeLimits returns the quota and limit errors that ended
// opencode turns of the sessions run in dir (all when empty), after the
// given time in ms, oldest first. Other errors are not limits. A limit
// with no reset in its message resets after the response's retry-after.
//
// opencode retries Zen's limits, waiting out their retry-after up to five
// times, so a Zen limit lands here only once those retries run out.
func ReadOpenCodeLimits(dbPath, dir string, afterMs int64) ([]OpenCodeLimit, error) {
	var out []OpenCodeLimit
	err := openCodeRows(dbPath, dir, afterMs, func(_ string, updated int64, m openCodeMessage) {
		if updated <= afterMs || m.Role != "assistant" || m.Error == nil {
			return
		}
		d := m.Error.Data
		l, ok := ParseOpenCodeLimit(d.Message)
		if !ok && !openCodeQuotaBody(d.ResponseBody) {
			return
		}
		if !ok {
			l = OpenCodeLimit{Message: d.Message}
		}
		if l.ResetIn == 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(d.ResponseHeaders["retry-after"])); err == nil && n > 0 {
				l.Resets = fmt.Sprintf("in %ds", n)
				l.ResetIn = time.Duration(n) * time.Second
			}
		}
		l.Provider, l.ModelID = m.ProviderID, m.ModelID
		l.Time = time.UnixMilli(updated)
		out = append(out, l)
	})
	return out, err
}
