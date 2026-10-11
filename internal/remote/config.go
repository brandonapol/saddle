package remote

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultListen is the loopback address the server binds unless told
// otherwise. Remote machines reach it through ssh -L or a tailnet proxy.
const DefaultListen = "127.0.0.1:7431"

// EnvDir overrides where tokens and the audit log live (tests, multiple
// users on one box).
const EnvDir = "SADDLE_REMOTE_DIR"

// Config is the [remote] section of the user's own config file.
type Config struct {
	// Enabled opts in. Off by default.
	Enabled bool `toml:"enabled"`
	// Listen is a host:port, loopback unless AllowPublic and TLS are set.
	Listen string `toml:"listen"`
	// AllowPublic lets Listen be an address that isn't loopback (a LAN or
	// tailnet IP, or 0.0.0.0). It also needs TLSCert and TLSKey.
	AllowPublic bool `toml:"allow_public"`
	// TLSCert and TLSKey are PEM files. When set, the server speaks only
	// TLS (1.2 or newer). The key must be 0600.
	TLSCert string `toml:"tls_cert"`
	TLSKey  string `toml:"tls_key"`
	// Hosts are extra names the Host header may carry (the box's DNS or
	// tailnet name). Loopback names, and the listen IP itself, always pass.
	// Anything else is refused, which keeps DNS rebinding out.
	Hosts []string `toml:"hosts"`
	// Repos are the repos one listener serves, by absolute path, each at
	// /mcp/<its directory name> (#312); the first is also at /mcp. Each
	// must be trusted (saddle trust). Empty serves the repo saddle runs
	// in. Set, it is also the list saddle remote stdio may serve.
	Repos []string `toml:"repos"`
	// AuditMaxMB rotates the audit log past this size (default 10).
	// AuditKeep is how many rotated logs are kept (default 5).
	AuditMaxMB int `toml:"audit_max_mb"`
	AuditKeep  int `toml:"audit_keep"`
	// PerMinute caps calls per token; FailsPerMinute caps failed logins
	// per client address.
	PerMinute      int `toml:"per_minute"`
	FailsPerMinute int `toml:"fails_per_minute"`
	// Push is [remote.push]: a phone notification for interrupt-class
	// notices. It is independent of Enabled: on exactly when it names a
	// target.
	Push PushConfig `toml:"push"`
}

// PushConfig is [remote.push]: where interrupt-class notices go. Set one
// of Ntfy or URL; neither means push is off.
type PushConfig struct {
	// Ntfy is an ntfy topic URL (https://ntfy.sh/your-secret-topic). The
	// summary is the body and the task is in the title.
	Ntfy string `toml:"ntfy"`
	// URL is a generic webhook: a JSON PushPayload is POSTed to it.
	URL string `toml:"url"`
	// Token, when set, is sent as a bearer Authorization header.
	Token string `toml:"token"`
}

// On reports whether push is configured.
func (p PushConfig) On() bool { return p.Ntfy != "" || p.URL != "" }

// target is where pushes go and whether it is ntfy.
func (p PushConfig) target() (string, bool) {
	if p.Ntfy != "" {
		return p.Ntfy, true
	}
	return p.URL, false
}

// Check refuses a push target that isn't https, except plain http to
// loopback, since a notice's text would cross the network in the clear.
func (p PushConfig) Check() error {
	if p.Ntfy != "" && p.URL != "" {
		return errors.New("[remote.push] takes ntfy or url, not both")
	}
	if !p.On() {
		return nil
	}
	raw, _ := p.target()
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("[remote.push] %q: %w", raw, err)
	}
	switch {
	case u.Scheme == "https" && u.Host != "":
		return nil
	case u.Scheme == "http" && loopbackHost(u.Hostname()):
		return nil
	}
	return fmt.Errorf("[remote.push] %q: want an https URL (plain http only to loopback)", raw)
}

// Limits returns the rate limits, defaulted.
func (c Config) Limits() Limits {
	l := Limits{PerMinute: c.PerMinute, FailsPerMinute: c.FailsPerMinute}
	if l.PerMinute <= 0 {
		l.PerMinute = 120
	}
	if l.FailsPerMinute <= 0 {
		l.FailsPerMinute = 10
	}
	return l
}

// AuditLimits returns the audit rotation settings, defaulted.
func (c Config) AuditLimits() (maxBytes int64, keep int) {
	maxBytes, keep = DefaultAuditMaxBytes, DefaultAuditKeep
	if c.AuditMaxMB > 0 {
		maxBytes = int64(c.AuditMaxMB) << 20
	}
	if c.AuditKeep > 0 {
		keep = c.AuditKeep
	}
	return maxBytes, keep
}

// UserConfigPath is ~/.config/saddle/config.toml (honoring XDG_CONFIG_HOME).
func UserConfigPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "saddle", "config.toml")
}

// Dir is where tokens and the audit log live: $SADDLE_REMOTE_DIR, else
// ~/.config/saddle/remote. Never inside a repo.
func Dir() (string, error) {
	if d := os.Getenv(EnvDir); d != "" {
		return d, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "saddle", "remote"), nil
}

// LoadConfig reads [remote] from the user config at path only. A repo's
// .saddle/config.toml is never consulted: a cloned repo must not be able to
// open a listener on its own.
func LoadConfig(path string) (Config, error) {
	var f struct {
		Remote Config `toml:"remote"`
	}
	f.Remote.Listen = DefaultListen
	if path != "" {
		if _, err := toml.DecodeFile(path, &f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return f.Remote, err
		}
	}
	if f.Remote.Listen == "" {
		f.Remote.Listen = DefaultListen
	}
	return f.Remote, nil
}

// repoName is what a repo's directory name must look like to be a path
// segment.
var repoName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// RepoList is Repos cleaned and checked: absolute paths ("~/" expands),
// with directory names that are unique and fit in a URL path.
func (c Config) RepoList() ([]string, error) {
	var out []string
	names := map[string]string{}
	for _, r := range c.Repos {
		if rest, ok := strings.CutPrefix(r, "~/"); ok {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			r = filepath.Join(home, rest)
		}
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("[remote] repos: %q isn't an absolute path", r)
		}
		r = filepath.Clean(r)
		n := filepath.Base(r)
		if !repoName.MatchString(n) || n == "." || n == ".." {
			return nil, fmt.Errorf("[remote] repos: %q: its name %q must be letters, digits, '.', '_' or '-' (it is served at /mcp/<name>)", r, n)
		}
		if prev, ok := names[n]; ok {
			return nil, fmt.Errorf("[remote] repos: %s and %s are both named %q; /mcp/%s can only be one of them", prev, r, n, n)
		}
		names[n] = r
		out = append(out, r)
	}
	return out, nil
}

// CheckListed refuses a repo that [remote] repos is set and doesn't name.
func (c Config) CheckListed(root string) error {
	rs, err := c.RepoList()
	if err != nil || len(rs) == 0 {
		return err
	}
	if slices.Contains(rs, filepath.Clean(root)) {
		return nil
	}
	return fmt.Errorf("%s isn't listed in [remote] repos in %s", root, UserConfigPath())
}

// Check refuses a bind that would expose saddle beyond this machine unless
// the owner asked for it twice: allow_public = true, and TLS. A loopback
// bind may use TLS too, but needn't. It also checks [remote] repos.
func (c Config) Check() error {
	if _, err := c.RepoList(); err != nil {
		return err
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("[remote] needs both tls_cert and tls_key, or neither")
	}
	if c.TLSCert != "" {
		if _, err := os.Stat(c.TLSCert); err != nil {
			return fmt.Errorf("tls_cert: %w", err)
		}
		fi, err := os.Stat(c.TLSKey)
		if err != nil {
			return fmt.Errorf("tls_key: %w", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("tls_key %s is readable by others (mode %v); run chmod 600 on it", c.TLSKey, fi.Mode().Perm())
		}
	}
	err := CheckListen(c.Listen)
	if err == nil || !errors.Is(err, errNotLoopback) {
		return err
	}
	if !c.AllowPublic {
		return fmt.Errorf("%w; to bind it anyway set allow_public = true and tls_cert/tls_key under [remote]", err)
	}
	if c.TLSCert == "" {
		return fmt.Errorf("listen %q: a public bind needs tls_cert and tls_key under [remote]", c.Listen)
	}
	return nil
}

// TLS reports whether the server speaks TLS.
func (c Config) TLS() bool { return c.TLSCert != "" }

// AllowedHosts are the Host header values the handler accepts besides
// loopback ones: the configured names, and the listen IP when it is one.
func (c Config) AllowedHosts() []string {
	hs := append([]string(nil), c.Hosts...)
	if h, _, err := net.SplitHostPort(c.Listen); err == nil {
		if ip := net.ParseIP(h); ip != nil && !ip.IsUnspecified() {
			hs = append(hs, h)
		}
	}
	return hs
}

// listen opens the listener: TLS 1.2 or newer when a cert is set, plain TCP
// otherwise. It doesn't Check; callers do that first.
func (c Config) listen() (net.Listener, error) {
	var tc *tls.Config
	if c.TLS() {
		cert, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
		if err != nil {
			return nil, err
		}
		tc = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return nil, err
	}
	if tc != nil {
		ln = tls.NewListener(ln, tc)
	}
	return ln, nil
}

var errNotLoopback = errors.New("only loopback addresses (127.0.0.1, ::1, localhost) are allowed; reach it from elsewhere with ssh -L")

// CheckListen refuses any address that isn't loopback. Config.Check lets a
// public bind through when allow_public and TLS are both set.
func CheckListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("listen %q: no port", addr)
	}
	if !loopbackHost(host) {
		return fmt.Errorf("listen %q: %w", addr, errNotLoopback)
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
