package remote

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/trust"
)

// AcquireServeLock takes the lock for serving on listen, recording holder
// in it, and returns its release. One process at a time serves an address:
// saddle up, the plugin engine or a foreground saddle remote serve. A held
// lock's error names its holder.
func AcquireServeLock(dir, listen, holder string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "serve-"+strings.NewReplacer(":", "_", "[", "", "]", "", "/", "_").Replace(listen)+".lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := os.ReadFile(p)
		_ = f.Close()
		who := strings.TrimSpace(string(b))
		if who == "" {
			who = "another saddle process"
		}
		return nil, fmt.Errorf("saddle remote is already serving %s: %s", listen, who)
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.WriteAt([]byte(holder+"\n"), 0); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// Service is the remote listener's lifecycle: check the bind, take the
// lock, listen (TLS when configured), serve until ctx ends, then shut down
// and free the lock.
type Service struct {
	Cfg     Config
	Handler http.Handler
	Dir     string // where the lock lives
	Holder  string // who is serving, recorded in the lock
	// Wait makes Run keep trying while another process holds the lock or
	// the address is busy, instead of failing. saddle up and the engine
	// wait, so a foreground serve can come and go.
	Wait  bool
	Retry time.Duration // between tries when waiting; default 30s
	// Logf reports why a waiting service isn't serving, once per reason.
	Logf    func(format string, args ...any)
	OnServe func(net.Addr)
}

// Run serves until ctx ends. It returns nil on shutdown, and an error when
// the config is refused or, unless Wait, when it can't serve.
func (s *Service) Run(ctx context.Context) error {
	if err := s.Cfg.Check(); err != nil {
		return err
	}
	retry := s.Retry
	if retry <= 0 {
		retry = 30 * time.Second
	}
	last := ""
	for {
		err := s.serveOnce(ctx)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		if !s.Wait {
			return err
		}
		if msg := err.Error(); msg != last && s.Logf != nil {
			last = msg
			s.Logf("remote control not serving yet: %s", msg)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retry):
		}
	}
}

func (s *Service) serveOnce(ctx context.Context) error {
	release, err := AcquireServeLock(s.Dir, s.Cfg.Listen, s.Holder)
	if err != nil {
		return err
	}
	defer release()
	ln, err := s.Cfg.listen()
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler, ReadHeaderTimeout: 10 * time.Second}
	if s.OnServe != nil {
		s.OnServe(ln.Addr())
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		<-errc
		return nil
	case err := <-errc:
		return err
	}
}

// appServer is what serving a repo needs: the handler and its audit log.
func appServer(a *app.App, cfg Config) (http.Handler, *Audit, string, error) {
	if rep, err := trust.Status(a.Root, nil); err != nil || rep.State != trust.StateTrusted {
		return nil, nil, "", errors.New("this repo isn't trusted: run saddle trust before exposing it")
	}
	dir, err := Dir()
	if err != nil {
		return nil, nil, "", err
	}
	audit, err := openAppAudit(a, dir, cfg)
	if err != nil {
		return nil, nil, "", err
	}
	h := Handler(AppSource{A: a}, NewTokens(dir), audit, Options{Limits: cfg.Limits(), Repo: a.Root, Hosts: cfg.AllowedHosts()})
	return h, audit, dir, nil
}

// openAppAudit opens the audit log with the configured rotation and mirrors
// it into the repo's event log.
func openAppAudit(a *app.App, dir string, cfg Config) (*Audit, error) {
	audit, err := OpenAudit(dir)
	if err != nil {
		return nil, err
	}
	audit.MaxBytes, audit.Keep = cfg.AuditLimits()
	audit.Mirror = func(e AuditEntry) {
		a.Store.Event(app.OrchestratorID, "remote", fmt.Sprintf("%s %s %s from %s %s", e.Token, e.Decision, e.Tool, e.Addr, e.Reason))
	}
	return audit, nil
}

// RunManaged serves remote control for the life of ctx when the user config
// enables it, and returns at once otherwise. saddle up and the plugin
// engine run it beside their other watchers. It waits, rather than fails,
// while a foreground saddle remote serve holds the address. Problems go to
// logf, or to the repo's event log when logf is nil; they never stop the
// caller.
func RunManaged(ctx context.Context, a *app.App, logf func(format string, args ...any)) {
	cfg, err := LoadConfig(UserConfigPath())
	if logf == nil && a != nil {
		logf = func(f string, args ...any) { a.Store.Event(app.OrchestratorID, "remote", fmt.Sprintf(f, args...)) }
	}
	if err != nil {
		if logf != nil {
			logf("remote control config: %v", err)
		}
		return
	}
	if !cfg.Enabled || a == nil {
		return
	}
	h, audit, dir, err := appServer(a, cfg)
	if err != nil {
		logf("remote control not serving: %v", err)
		return
	}
	defer func() { _ = audit.Close() }()
	s := &Service{Cfg: cfg, Handler: h, Dir: dir, Wait: true, Logf: logf,
		Holder: fmt.Sprintf("saddle (pid %d) for %s", os.Getpid(), a.Root),
		OnServe: func(addr net.Addr) {
			logf("remote control serving %s at %s", a.Root, serveURL(cfg, addr))
		}}
	if err := s.Run(ctx); err != nil {
		logf("remote control not serving: %v", err)
	}
}

func serveURL(cfg Config, addr net.Addr) string {
	scheme := "http"
	if cfg.TLS() {
		scheme = "https"
	}
	return scheme + "://" + addr.String() + MCPPath
}

// StdioCaller is who an ssh forced-command session runs as: the scopes the
// forced command pins, named ssh:NAME, from the client address in
// SSH_CONNECTION ("client-ip client-port server-ip server-port").
func StdioCaller(scopes []Scope, name, sshConnection string) caller {
	tn := "ssh"
	if name != "" {
		tn += ":" + name
	}
	addr := "stdio"
	if f := strings.Fields(sshConnection); len(f) > 0 {
		addr = f[0]
	}
	return caller{tok: Token{Name: tn, Hash: "stdio:" + tn, Scopes: scopes}, addr: addr}
}

// ServeStdio serves the same scoped server Handler does over one
// transport (stdin and stdout under ssh) until the client goes away.
func ServeStdio(ctx context.Context, src Source, audit *Audit, c caller, t mcp.Transport) error {
	return newServer(src, c, audit, newServerState(Options{})).Run(ctx, t)
}
