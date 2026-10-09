//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// lazyOrchestrator replaces the world's claude with one whose headless
// orchestrator acts like Claude Code 2.1.293 where the shared fake doesn't:
// system/init comes only with the first message, /clear and /compact
// answer with conversation_reset and compact_boundary, and "retry:" sits
// in API retries for a few seconds. Worker launches still run the fake
// agent. It returns the file each orchestrator process logs its pid to.
func lazyOrchestrator(t *testing.T, w *World) string {
	t.Helper()
	pids := filepath.Join(w.Root, "orch-pids")
	real, err := os.ReadFile(filepath.Join(w.Bin, "claude"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(w.Bin, "claude-agent"), real, 0o755))
	script := `#!/bin/sh
case " $* " in *" --input-format "*) ;; *) exec ` + shq(filepath.Join(w.Bin, "claude-agent")) + ` "$@" ;; esac
echo $$ >> ` + shq(pids) + `
say() { printf '%s\n' "$1"; }
result() { say '{"type":"result","subtype":"success","result":"","session_id":"lazy-orch","total_cost_usd":0.01}'; }
inited=
while IFS= read -r line; do
	case "$line" in
	*'"control_request"'*)
		id=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
		say '{"type":"control_response","response":{"subtype":"success","request_id":"'"$id"'","response":{"commands":[]}}}'
		continue ;;
	esac
	if [ -z "$inited" ]; then
		say '{"type":"system","subtype":"init","session_id":"lazy-orch","model":"lazy-model"}'
		inited=1
	fi
	case "$line" in
	*'"/clear"'*)
		say '{"type":"conversation_reset","new_conversation_id":"c2","trigger":"clear","session_id":"lazy-orch"}'
		result ;;
	*'"/compact"'*)
		say '{"type":"system","subtype":"compact_boundary","session_id":"lazy-orch","compact_metadata":{"trigger":"manual"}}'
		result ;;
	*'retry:'*)
		say '{"type":"system","subtype":"api_retry","attempt":3,"max_retries":10,"retry_delay_ms":4000,"error_status":null,"error":"unknown"}'
		sleep 4
		say '{"type":"assistant","message":{"content":[{"type":"text","text":"lazy answer after retries"}]}}'
		result ;;
	*)
		say '{"type":"assistant","message":{"content":[{"type":"text","text":"lazy ack"}]}}'
		result ;;
	esac
done
`
	must(t, os.WriteFile(filepath.Join(w.Bin, "claude"), []byte(script), 0o755))
	return pids
}

// liveOrchestrators counts the logged orchestrator processes still alive.
func liveOrchestrators(pids string) int {
	b, _ := os.ReadFile(pids)
	n := 0
	for _, f := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(f)
		if err == nil && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			n++
		}
	}
	return n
}

// TestJourneyTUIFixes2 covers four TUI fixes on the real binary:
//   - #259: ctrl+r twice resumes without "Couldn't resume" and leaves one
//     orchestrator process;
//   - #261: /compact and /clear say what they did, and /clear hides the old
//     transcript;
//   - #262: API retries show in place of "thinking…";
//   - #264: x and L ask first, and any key but y keeps things as they are.
func TestJourneyTUIFixes2(t *testing.T) {
	w := world(t, Options{})
	idleAgent(w, "t1", "Keep me alive", "keep/**")
	pids := lazyOrchestrator(t, w)
	u := w.StartTUI(160, 44)
	u.WaitScreen("Keep me alive")

	u.Type("hello")
	u.Keys("Enter")
	u.WaitScreen("lazy ack")

	// #259
	u.Keys("C-r")
	u.WaitScreen("Orchestrator restarted.")
	u.Keys("C-r")
	Eventually(t, "one live orchestrator", func() error {
		if n := liveOrchestrators(pids); n != 1 {
			return errorf("%d live orchestrator processes", n)
		}
		return nil
	})
	if s := u.Screen(); strings.Contains(s, "Couldn't resume") {
		t.Fatalf("ctrl+r read as a failed resume:\n%s", s)
	}
	u.Type("still there")
	u.Keys("Enter")
	u.WaitScreen("you", "still there")
	if n := liveOrchestrators(pids); n != 1 {
		t.Fatalf("%d live orchestrator processes after a message", n)
	}

	// #262
	u.Type("retry: please")
	u.Keys("Enter")
	u.WaitScreen("retrying, attempt 3/10")
	u.WaitScreen("lazy answer after retries")
	u.WaitGone("retrying, attempt")

	// #261
	u.Type("/compact")
	u.Keys("Enter")
	u.WaitScreen("Conversation compacted.")
	u.Type("/clear")
	u.Keys("Enter")
	u.WaitScreen("Conversation cleared.")
	u.WaitGone("lazy answer after retries")

	// #264
	u.Keys("Tab")
	u.Type("x")
	u.WaitScreen("Kill t1?")
	u.Type("n")
	u.WaitScreen("kept t1")
	if v := w.Task("t1"); v.Status != "idle" {
		t.Fatalf("t1 after declining the kill = %+v", v)
	}
	u.Type("L")
	u.WaitScreen("Land the queued branches now?")
	u.Keys("Escape")
	u.WaitScreen("land cancelled")
	u.Quit()
}
