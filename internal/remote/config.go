package remote

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
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
	// Listen is a loopback host:port.
	Listen string `toml:"listen"`
	// PerMinute caps calls per token; FailsPerMinute caps failed logins
	// per client address.
	PerMinute      int `toml:"per_minute"`
	FailsPerMinute int `toml:"fails_per_minute"`
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

// CheckListen refuses any address that isn't loopback. A public or LAN
// listener needs TLS and is out of scope for the spike.
func CheckListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen %q: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("listen %q: no port", addr)
	}
	if !loopbackHost(host) {
		return fmt.Errorf("listen %q: only loopback addresses (127.0.0.1, ::1, localhost) are allowed; reach it from elsewhere with ssh -L", addr)
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
