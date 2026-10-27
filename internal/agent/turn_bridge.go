package agent

import (
	"bufio"
	"io"
	"strings"
)

// bridgeCompactPrompt asks the old session for the summary a fresh one starts from.
const bridgeCompactPrompt = "[saddle] This session is being compacted: a fresh session will replace it and see only your reply. " +
	"Reply with a short summary of where things stand: the owner's open requests, the tasks in flight and anything you promised to do. " +
	"Reply with the summary only."

// bridgeTurn is one headless turn's outcome.
type bridgeTurn struct {
	session string // the session the CLI ran the turn on; "" when it never started
	text    string // the last assistant message
	err     error
}

// turnBridge drives a CLI that runs one headless turn per process and
// resumes a session by id: codex exec (#506) and opencode run (#745). It
// keeps the session across turns, starts fresh when the stored one won't
// resume, opens each new session with the brief, and emulates /compact
// with a summary turn and a fresh session started from it.
type turnBridge struct {
	brief   string
	model   string
	w       io.Writer
	resume  string
	summary string // carried into the next fresh session after a compaction
	// run runs one turn on resume ("" starts a session). With forward
	// set, its events go to w as stream-json.
	run func(prompt, resume string, forward bool) bridgeTurn
}

// serve reads Claude-style stream-json user lines from r and runs a turn
// for each, or a compaction for a /compact line.
func (br *turnBridge) serve(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		text, ok := userText(sc.Bytes())
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		if keep, ok := compactRequest(text); ok {
			br.compact(keep)
			continue
		}
		br.turn(text)
	}
	return sc.Err()
}

// turn runs one user message, starting a fresh session when the stored one
// can't be resumed.
func (br *turnBridge) turn(text string) {
	res := br.run(br.prompt(text), br.resume, true)
	if res.err != nil && br.resume != "" && res.session == "" {
		// A session id from another adapter, or a deleted session, should
		// not wedge the chat. One retry starts fresh.
		br.resume = ""
		res = br.run(br.prompt(text), "", true)
	}
	if res.session != "" {
		if res.session != br.resume {
			br.summary = ""
		}
		br.resume = res.session
	}
	if res.err != nil {
		writeResult(br.w, true, res.err.Error(), br.resume)
	}
}

// prompt is text, preceded in a new session by the brief and any summary of
// the session it replaces.
func (br *turnBridge) prompt(text string) string {
	if br.resume != "" {
		return text
	}
	var b strings.Builder
	if s := strings.TrimSpace(br.brief); s != "" {
		b.WriteString(s + "\n\n")
	}
	if br.summary != "" {
		b.WriteString("[saddle] This session continues one that was compacted. Its summary:\n" + br.summary + "\n\n")
	}
	b.WriteString(text)
	return b.String()
}

// compact asks the session for a summary, then starts a fresh session from
// it and reports the boundary, as Claude Code does for /compact.
func (br *turnBridge) compact(keep string) {
	if br.resume == "" {
		writeResult(br.w, false, "Nothing to compact yet.", "")
		return
	}
	ask := bridgeCompactPrompt
	if keep != "" {
		ask += " Keep: " + keep
	}
	old := br.run(ask, br.resume, false)
	if old.err != nil {
		writeResult(br.w, true, "compact: "+old.err.Error(), br.resume)
		return
	}
	br.summary = strings.TrimSpace(old.text)
	br.resume = ""
	fresh := br.run(br.prompt("[saddle] Reply with the word ready, then wait for the owner's next message."), "", false)
	if fresh.err != nil {
		writeResult(br.w, true, "compact: "+fresh.err.Error(), "")
		return
	}
	br.resume = fresh.session
	for _, l := range []map[string]any{
		{"type": "system", "subtype": "init", "session_id": fresh.session, "model": br.model},
		{"type": "system", "subtype": "compact_boundary", "session_id": fresh.session,
			"compact_metadata": map[string]any{"trigger": "manual"}},
		{"type": "result", "subtype": "success", "is_error": false, "result": "Compacted.", "session_id": fresh.session},
	} {
		if writeLine(br.w, l) != nil {
			return
		}
	}
}

// compactRequest reports whether text is a /compact command, and its
// instruction.
func compactRequest(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if t != "/compact" && !strings.HasPrefix(t, "/compact ") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(t, "/compact")), true
}

// stderrTail collects the last 4KB of a turn's stderr.
func stderrTail(r io.Reader) <-chan string {
	ch := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		if len(b) > 4096 {
			b = b[len(b)-4096:]
		}
		ch <- strings.TrimSpace(string(b))
	}()
	return ch
}
