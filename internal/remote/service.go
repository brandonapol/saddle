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
	"sync"
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
	name := "serve-" + strings.NewReplacer(":", "_", "[", "", "]", "", "/", "_").Replace(listen) + ".lock"
	return acquireLock(dir, name, holder, "saddle remote is already serving "+listen)
}

// acquireLock takes the flock on dir/name, recording holder in it. A held
// lock's error is busy plus its holder.
func acquireLock(dir, name, holder, busy string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, name)
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
		return nil, fmt.Errorf("%s: %s", busy, who)
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

// hosted is what serving needs: the handler, its audit log, and the repos
// it serves. close closes the repos it opened besides the host's.
type hosted struct {
	h     http.Handler
	audit *Audit
	dir   string
	repos []ServedRepo
	close func()
}

// trusted refuses a repo saddle trust (#215) hasn't trusted. The listener
// asks it on every request, so revoking trust stops serving at once.
func trusted(root string) error {
	rep, err := trust.Status(root, nil)
	if err != nil {
		return err
	}
	if rep.State != trust.StateTrusted {
		return fmt.Errorf("state %s; run saddle trust in %s", rep.State, root)
	}
	return nil
}

// appServer builds the handler for the repos cfg serves: the repo a runs
// in, or every repo in [remote] repos (#312), each at /mcp/<name>.
func appServer(a *app.App, cfg Config, logf func(format string, args ...any)) (*hosted, error) {
	roots, err := cfg.RepoList()
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		if err := trusted(a.Root); err != nil {
			return nil, errors.New("this repo isn't trusted: run saddle trust before exposing it")
		}
	}
	repos, apps := servedRepos(a, roots, app.Open, logf)
	if len(repos) == 0 {
		return nil, errors.New("none of [remote] repos could be opened")
	}
	closeApps := func() {
		for _, x := range apps {
			_ = x.Close()
		}
	}
	dir, err := Dir()
	if err != nil {
		closeApps()
		return nil, err
	}
	byName := map[string]*app.App{}
	for _, x := range append([]*app.App{a}, apps...) {
		byName[filepath.Base(x.Root)] = x
	}
	audit, err := openAudit(dir, cfg, func(name string) *app.App {
		if x := byName[name]; x != nil {
			return x
		}
		return a
	})
	if err != nil {
		closeApps()
		return nil, err
	}
	h := ReposHandler(repos, NewTokens(dir), audit, Options{Limits: cfg.Limits(), Hosts: cfg.AllowedHosts(), Trusted: trusted})
	return &hosted{h: h, audit: audit, dir: dir, repos: repos, close: closeApps}, nil
}

// servedRepos opens the repos at roots, reusing a for its own, and skips
// (and reports) one that can't be opened. With no roots it serves a alone.
// apps are the ones it opened, for the caller to close.
func servedRepos(a *app.App, roots []string, open func(string) (*app.App, error), logf func(string, ...any)) (repos []ServedRepo, apps []*app.App) {
	if len(roots) == 0 {
		return []ServedRepo{{Name: filepath.Base(a.Root), Root: a.Root, Src: AppSource{A: a}}}, nil
	}
	for _, root := range roots {
		x := a
		if filepath.Clean(a.Root) != root {
			o, err := open(root)
			switch {
			case err != nil:
				logf("remote control: not serving %s: %v", root, err)
				continue
			case filepath.Clean(o.Root) != root:
				// SADDLE_ROOT, or a path inside a repo, opened another one.
				logf("remote control: not serving %s: it opened %s instead", root, o.Root)
				_ = o.Close()
				continue
			}
			x = o
			apps = append(apps, o)
		}
		if err := trusted(root); err != nil {
			logf("remote control: %s isn't trusted (%v); its requests are refused until it is", root, err)
		}
		repos = append(repos, ServedRepo{Name: filepath.Base(root), Root: root, Src: AppSource{A: x}})
	}
	return repos, apps
}

// openAppAudit opens the audit log with the configured rotation and mirrors
// it into the repo's event log.
func openAppAudit(a *app.App, dir string, cfg Config) (*Audit, error) {
	return openAudit(dir, cfg, func(string) *app.App { return a })
}

// openAudit opens the audit log with the configured rotation and mirrors
// each entry into the event log of the repo it names (appFor).
func openAudit(dir string, cfg Config, appFor func(repo string) *app.App) (*Audit, error) {
	audit, err := OpenAudit(dir)
	if err != nil {
		return nil, err
	}
	audit.MaxBytes, audit.Keep = cfg.AuditLimits()
	audit.Mirror = func(e AuditEntry) {
		appFor(e.Repo).Store.Event(app.OrchestratorID, "remote", fmt.Sprintf("%s %s %s from %s %s", e.Token, e.Decision, e.Tool, e.Addr, e.Reason))
	}
	return audit, nil
}

// RunManaged serves remote control for the life of ctx when the user config
// enables it, and runs [remote.push] when that is configured; with neither
// it returns at once. saddle up and the plugin engine run it beside their
// other watchers. It waits, rather than fails, while a foreground saddle
// remote serve holds the address. Problems go to
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
	if a == nil {
		return
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	if cfg.Push.On() {
		wg.Go(func() { RunPush(ctx, a, cfg.Push, "", logf) })
	}
	if !cfg.Enabled {
		return
	}
	hs, err := appServer(a, cfg, logf)
	if err != nil {
		logf("remote control not serving: %v", err)
		return
	}
	defer hs.close()
	defer func() { _ = hs.audit.Close() }()
	s := &Service{Cfg: cfg, Handler: hs.h, Dir: hs.dir, Wait: true, Logf: logf,
		Holder: fmt.Sprintf("saddle (pid %d) for %s", os.Getpid(), a.Root),
		OnServe: func(addr net.Addr) {
			for _, r := range hs.repos {
				logf("remote control serving %s at %s", r.Root, repoURL(cfg, addr, r.Name))
			}
		}}
	if err := s.Run(ctx); err != nil {
		logf("remote control not serving: %v", err)
	}
}

// repoURL is where a served repo answers.
func repoURL(cfg Config, addr net.Addr, name string) string {
	scheme := "http"
	if cfg.TLS() {
		scheme = "https"
	}
	return scheme + "://" + addr.String() + MCPPath + "/" + name
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
	return newServer(&route{ServedRepo: ServedRepo{Src: src}}, c, audit, newServerState(Options{})).Run(ctx, t)
}
