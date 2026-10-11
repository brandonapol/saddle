package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// maxPushSummary caps a push's one line.
const maxPushSummary = 200

// pushEvery is how often the pusher reads new events.
const pushEvery = 5 * time.Second

// PushPayload is what the generic webhook receives. It never carries peek
// output: only the task and the first line of the notice, scrubbed.
type PushPayload struct {
	Repo    string    `json:"repo"`
	Task    string    `json:"task"`
	Class   string    `json:"class"` // always "interrupt"
	Summary string    `json:"summary"`
	TS      time.Time `json:"ts"`
}

// Pusher sends interrupt-class orchestrator notices (#222) to the
// [remote.push] target. It follows the event log, where the notice policy
// records each notice with its class, from the moment it starts: history
// is never replayed, and digest and silent notices are never sent.
type Pusher struct {
	st     *store.Store
	cfg    PushConfig
	repo   string
	after  int64
	client *http.Client
}

// NewPusher starts a pusher at the end of the event log.
func NewPusher(st *store.Store, cfg PushConfig, repo string) (*Pusher, error) {
	if err := cfg.Check(); err != nil {
		return nil, err
	}
	after, err := st.LastEventID()
	if err != nil {
		return nil, err
	}
	return &Pusher{st: st, cfg: cfg, repo: repo, after: after, client: &http.Client{
		Timeout: 10 * time.Second,
		// A redirect could downgrade to http or leave the configured host.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Step sends every interrupt notice logged since the last step. A send
// that fails is reported, not retried: the notice still waits in the
// orchestrator's queue and in needs_you.
func (p *Pusher) Step(ctx context.Context) error {
	evs, err := p.st.EventsSince(p.after)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range evs {
		p.after = e.ID
		if e.Task != app.OrchestratorID || e.Kind != app.EventNoticeInterrupt {
			continue
		}
		errs = append(errs, p.send(ctx, e))
	}
	return errors.Join(errs...)
}

func (p *Pusher) send(ctx context.Context, e store.Event) error {
	first, _, _ := strings.Cut(strings.TrimSpace(e.Data), "\n")
	pl := PushPayload{Repo: p.repo, Task: pushTask(e.Data), Class: string(app.NoticeInterrupt),
		Summary: scrubText(strings.TrimSpace(first), maxPushSummary), TS: e.TS}
	target, ntfy := p.cfg.target()
	var body []byte
	if ntfy {
		body = []byte(pl.Summary)
	} else {
		body, _ = json.Marshal(pl)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if ntfy {
		req.Header.Set("Title", fmt.Sprintf("saddle %s: %s", pl.Repo, pl.Task))
		req.Header.Set("Tags", "saddle")
		req.Header.Set("Priority", "high")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.Token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("push %s: %w", pl.Task, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("push %s: %s", pl.Task, resp.Status)
	}
	return nil
}

var taskIDRe = regexp.MustCompile(`\bt\d+\b`)

// pushTask is the task a notice is about: the first task id in it, else
// the orchestrator.
func pushTask(text string) string {
	if m := taskIDRe.FindString(text); m != "" {
		return m
	}
	return app.OrchestratorID
}

// acquirePushLock takes the lock for pushing a repo's notices. saddle up
// and the plugin engine may both run RunPush; only the holder sends.
func acquirePushLock(dir, root, holder string) (func(), error) {
	h := sha256.Sum256([]byte(filepath.Clean(root)))
	return acquireLock(dir, "push-"+hex.EncodeToString(h[:8])+".lock", holder, "already pushing notices for "+filepath.Base(root))
}

// RunPush sends a repo's interrupt notices to cfg's target for the life of
// ctx. It returns at once when push is off. While another process pushes
// for the repo it waits and retries. Problems go to logf, once per reason.
func RunPush(ctx context.Context, a *app.App, cfg PushConfig, dir string, logf func(format string, args ...any)) {
	if !cfg.On() || a == nil {
		return
	}
	last := ""
	report := func(err error) {
		if msg := err.Error(); msg != last {
			last = msg
			logf("remote push: %v", err)
		}
	}
	if err := cfg.Check(); err != nil {
		report(err)
		return
	}
	if dir == "" {
		d, err := Dir()
		if err != nil {
			report(err)
			return
		}
		dir = d
	}
	for {
		release, err := acquirePushLock(dir, a.Root, fmt.Sprintf("saddle (pid %d) for %s", os.Getpid(), a.Root))
		if err == nil {
			err = pushUntilDone(ctx, a, cfg, report)
			release()
			if err == nil {
				return
			}
		}
		report(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func pushUntilDone(ctx context.Context, a *app.App, cfg PushConfig, report func(error)) error {
	p, err := NewPusher(a.Store, cfg, filepath.Base(a.Root))
	if err != nil {
		return err
	}
	t := time.NewTicker(pushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := p.Step(ctx); err != nil && ctx.Err() == nil {
				report(err)
			}
		}
	}
}
