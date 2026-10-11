//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	harness "github.com/brandonapol/saddle/internal/e2e"
	"github.com/brandonapol/saddle/internal/remote"
)

// concurrencyLimit is the bots limit in force on the host, read in process.
func (h *host) concurrencyLimit() int {
	h.T.Helper()
	c, err := h.App().Concurrency()
	if err != nil {
		h.T.Fatal(err)
	}
	return c.Limit
}

// TestRemoteTwoControllersRace: two admin controllers on the real listener
// set the bots limit from the same version and confirm at the same time.
// Exactly one wins; the other is refused with the moved version instead of
// undoing it, and both calls are audited.
func TestRemoteTwoControllersRace(t *testing.T) {
	h := newHost(t)
	phone := h.mustConnect(h.token("phone", "admin"))
	laptop := h.mustConnect(h.token("laptop", "admin"))

	v := status(t, phone).Versions[remote.ObjConcurrency]
	if v == "" || status(t, laptop).Versions[remote.ObjConcurrency] != v {
		t.Fatalf("controllers see different concurrency versions")
	}
	before := h.concurrencyLimit()
	limits := map[string]int{"phone": before + 2, "laptop": before + 3}
	codes := map[string]string{
		"phone":  hold(t, phone, "concurrency", map[string]any{"limit": limits["phone"], "if_version": v}),
		"laptop": hold(t, laptop, "concurrency", map[string]any{"limit": limits["laptop"], "if_version": v}),
	}
	if got := h.concurrencyLimit(); got != before {
		t.Fatalf("a held call ran before its confirm: limit %d, want %d", got, before)
	}

	sessions := map[string]*mcp.ClientSession{"phone": phone, "laptop": laptop}
	errs := map[string]error{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for name, cs := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := confirm(cs, codes[name])
			mu.Lock()
			errs[name] = err
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	var won, lost []string
	for name, err := range errs {
		if err == nil {
			won = append(won, name)
		} else {
			lost = append(lost, name)
		}
	}
	if len(won) != 1 || len(lost) != 1 {
		t.Fatalf("want one winner and one refusal, got %v", errs)
	}
	if msg := errs[lost[0]].Error(); !strings.Contains(msg, "moved since you looked") {
		t.Fatalf("%s was refused for the wrong reason: %s", lost[0], msg)
	}
	if got := h.concurrencyLimit(); got != limits[won[0]] {
		t.Fatalf("limit = %d, want %s's %d (the loser must not undo it)", got, won[0], limits[won[0]])
	}
	if status(t, laptop).Versions[remote.ObjConcurrency] == v {
		t.Fatal("the concurrency version didn't move after a change")
	}
	h.waitAudit(won[0]+"'s confirmed change", func(e remote.AuditEntry) bool {
		return e.Token == won[0] && e.Tool == "concurrency" && e.Decision == remote.DecisionAllowed
	})
	h.waitAudit(lost[0]+"'s held change", func(e remote.AuditEntry) bool {
		return e.Token == lost[0] && e.Tool == "concurrency" && e.Decision == remote.DecisionPending
	})
}

// TestRemoteRevokedToken: revoking a token on the host cuts off a session
// already connected over the listener on its next request, a fresh
// connection is refused, and a confirm code held before the revoke can't
// be used. The other token keeps working, and the refusals are audited.
func TestRemoteRevokedToken(t *testing.T) {
	h := newHost(t)
	phoneSecret := h.token("phone", "admin")
	phone := h.mustConnect(phoneSecret)
	laptop := h.mustConnect(h.token("laptop", "read"))
	s := status(t, phone)
	before := h.concurrencyLimit()
	code := hold(t, phone, "concurrency", map[string]any{"limit": before + 1, "if_version": s.Versions[remote.ObjConcurrency]})

	deniedBefore := countDenied(h.audit())
	h.MustSaddle("remote", "token", "revoke", "phone")

	if _, err := call(phone, "status", nil); err == nil {
		t.Fatal("a revoked token's open session still reads status")
	}
	if _, err := confirm(phone, code); err == nil {
		t.Fatal("a revoked token confirmed a held call")
	}
	if got := h.concurrencyLimit(); got != before {
		t.Fatalf("limit = %d after a revoked confirm, want %d", got, before)
	}
	if _, err := h.connect(phoneSecret); err == nil {
		t.Fatal("a revoked token connected")
	}
	status(t, laptop) // another token is unaffected

	harness.Eventually(t, "the revoked token's refusals in the audit log", func() error {
		if n := countDenied(h.audit()); n < deniedBefore+3 {
			return fmt.Errorf("%d denied entries, want at least %d", n, deniedBefore+3)
		}
		return nil
	})
	r := h.MustSaddle("remote", "token", "list")
	if !strings.Contains(r.Stdout, "revoked") {
		t.Fatalf("token list doesn't show the revoke:\n%s", r.Stdout)
	}
}

func countDenied(es []remote.AuditEntry) int {
	n := 0
	for _, e := range es {
		if e.Decision == remote.DecisionDenied {
			n++
		}
	}
	return n
}

// TestRemotePanicPause: an act token presses pause over the listener with
// no confirm. Auto-merge stops on the host, every remote action (even an
// admin's confirm of a call held before the pause) is refused, reads keep
// working, and no remote tool can lift it. `saddle remote resume` on the
// host restores auto-merge and actions.
func TestRemotePanicPause(t *testing.T) {
	h := newHost(t)
	h.MustMCP("t0", "automerge", map[string]any{"action": "on"})
	automergeOn := func() bool {
		st, err := h.App().AutomergeState()
		if err != nil {
			t.Fatal(err)
		}
		return st.Enabled
	}
	if !automergeOn() {
		t.Fatal("auto-merge didn't turn on")
	}
	phone := h.mustConnect(h.token("phone", "act"))
	admin := h.mustConnect(h.token("laptop", "admin"))
	before := h.concurrencyLimit()
	code := hold(t, admin, "concurrency", map[string]any{"limit": before + 1,
		"if_version": status(t, admin).Versions[remote.ObjConcurrency]})

	if _, err := call(phone, remote.PauseTool, map[string]any{"reason": "e2e panic"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if automergeOn() {
		t.Fatal("auto-merge is still on after the pause")
	}
	s := status(t, admin)
	if s.Paused == nil || s.Paused.By != "remote:phone" || s.Paused.Reason != "e2e panic" {
		t.Fatalf("status.paused = %+v", s.Paused)
	}
	if _, err := confirm(admin, code); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("confirm during the pause: %v", err)
	}
	if _, err := call(admin, "concurrency", map[string]any{"limit": before + 1,
		"if_version": s.Versions[remote.ObjConcurrency]}); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("a new action during the pause: %v", err)
	}
	if _, err := call(phone, "needs_you", nil); err != nil {
		t.Fatalf("a read during the pause: %v", err)
	}
	if got := h.concurrencyLimit(); got != before {
		t.Fatalf("limit = %d during the pause, want %d", got, before)
	}
	for _, cs := range []*mcp.ClientSession{phone, admin} {
		res, err := cs.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range res.Tools {
			if strings.Contains(tool.Name, "resume") {
				t.Fatalf("a remote tool can resume: %s", tool.Name)
			}
		}
	}

	r := h.MustSaddle("remote", "resume")
	if !strings.Contains(r.Stdout, "resumed") {
		t.Fatalf("resume: %s", r)
	}
	if !automergeOn() {
		t.Fatal("resume didn't restore auto-merge")
	}
	s = status(t, admin)
	if s.Paused != nil {
		t.Fatalf("still paused after resume: %+v", s.Paused)
	}
	code = hold(t, admin, "concurrency", map[string]any{"limit": before + 1, "if_version": s.Versions[remote.ObjConcurrency]})
	if _, err := confirm(admin, code); err != nil {
		t.Fatalf("an action after resume: %v", err)
	}
	if got := h.concurrencyLimit(); got != before+1 {
		t.Fatalf("limit = %d after resume, want %d", got, before+1)
	}
}

// TestRemoteReadTokenDeniedLand: a read token over the listener isn't
// shown land, and calling it by name anyway is refused before anything
// runs and audited with the scope reason. Reads still work.
func TestRemoteReadTokenDeniedLand(t *testing.T) {
	h := newHost(t)
	cs := h.mustConnect(h.token("viewer", "read"))
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	for _, hidden := range []string{"land", "prs", "restack", remote.ConfirmTool, remote.PauseTool} {
		if slices.Contains(names, hidden) {
			t.Errorf("a read token is shown %s: %v", hidden, names)
		}
	}
	if !slices.Contains(names, "status") {
		t.Fatalf("a read token isn't shown status: %v", names)
	}

	if _, err := call(cs, "land", map[string]any{"if_version": status(t, cs).Versions["queue"]}); err == nil ||
		!strings.Contains(err.Error(), "lacks the scope") {
		t.Fatalf("land with a read token: %v", err)
	}
	h.waitAudit("the denied land", func(e remote.AuditEntry) bool {
		return e.Token == "viewer" && e.Tool == "land" && e.Decision == remote.DecisionDenied &&
			strings.Contains(e.Reason, "lacks the scope")
	})
	for _, e := range h.audit() {
		if e.Tool == "land" && e.Decision != remote.DecisionDenied {
			t.Fatalf("land got past the scope check: %+v", e)
		}
	}
	status(t, cs)
}
