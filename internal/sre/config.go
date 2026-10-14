// Package sre holds saddle's SRE mode: the per-repo .saddle/sre.toml config
// of known checks, endpoints and credential references (#547, #548).
package sre

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/brandonapol/saddle/internal/secretscan"
)

// File is the config path relative to the repo root.
const File = ".saddle/sre.toml"

// Config is the parsed .saddle/sre.toml.
type Config struct {
	Checks      []Check               `toml:"check"`
	Endpoints   map[string]Endpoint   `toml:"endpoints"`
	Credentials map[string]Credential `toml:"credentials"`
}

// Check is one known problem to check preemptively. It runs read-only: either
// Command (argv) or Surface (a named place to look, e.g. "endpoint:api"),
// and the result is compared with Expect.
type Check struct {
	Name        string   `toml:"name"`
	Description string   `toml:"description"`
	Command     []string `toml:"command"`
	Surface     string   `toml:"surface"`
	Expect      string   `toml:"expect"`
	Credentials []string `toml:"credentials"`
}

// Endpoint is a production endpoint. Credential names an entry in Credentials.
type Endpoint struct {
	URL        string `toml:"url"`
	Credential string `toml:"credential"`
}

// Credential is a reference to a secret, never the secret itself: Env is an
// environment variable name, Ref a "scheme:value" context or secret-manager
// reference (kube-context:prod, vault:secret/app, ...).
type Credential struct {
	Env string `toml:"env"`
	Ref string `toml:"ref"`
}

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	envRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	refRe  = regexp.MustCompile(`^[a-z][a-z0-9+.-]*:\S+$`)
	// secretKeyRe matches key names that would hold a literal secret.
	secretKeyRe = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api_?key|private_?key|credential_?value)$`)
	// secretFlagRe matches "--password=value" style inline arguments.
	secretFlagRe = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[-_]?key)=\S`)
)

// Load reads and validates <root>/.saddle/sre.toml. A missing file is an
// error wrapping os.ErrNotExist.
func Load(root string) (*Config, error) {
	b, err := os.ReadFile(filepath.Join(root, File))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	return Parse(b)
}

// Parse validates b as an sre.toml. Errors never echo secret values.
func Parse(b []byte) (*Config, error) {
	if len(secretscan.Find(b)) > 0 {
		return nil, fmt.Errorf("%s: contains a plaintext secret; reference it by env var name or ref instead", File)
	}
	var c Config
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	var errs []error
	for _, k := range md.Undecoded() {
		if secretKeyRe.MatchString(k[len(k)-1]) {
			errs = append(errs, fmt.Errorf("%s: plaintext secret field %q: store an env var name or ref, not the value", File, k))
		} else {
			errs = append(errs, fmt.Errorf("%s: unknown key %q", File, k))
		}
	}
	errs = append(errs, c.validate()...)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(File+": "+format, a...)) }

	for _, n := range sortedKeys(c.Credentials) {
		cr := c.Credentials[n]
		if (cr.Env == "") == (cr.Ref == "") {
			bad("credentials.%s: set exactly one of env or ref", n)
		}
		if cr.Env != "" && !envRe.MatchString(cr.Env) {
			bad("credentials.%s: env must be an environment variable name; a plaintext secret does not belong here", n)
		}
		if cr.Ref != "" && !refRe.MatchString(cr.Ref) {
			bad("credentials.%s: ref must look like scheme:value (e.g. kube-context:prod)", n)
		}
	}
	for _, n := range sortedKeys(c.Endpoints) {
		e := c.Endpoints[n]
		u, err := url.Parse(e.URL)
		switch {
		case e.URL == "":
			bad("endpoints.%s: url is required", n)
		case err != nil || u.Scheme == "" || u.Host == "":
			bad("endpoints.%s: url is not a valid absolute URL", n)
		default:
			if _, has := u.User.Password(); has {
				bad("endpoints.%s: url holds a plaintext secret (password); reference a credential instead", n)
			}
		}
		if e.Credential != "" {
			if _, ok := c.Credentials[e.Credential]; !ok {
				bad("endpoints.%s: credential %q is not defined", n, e.Credential)
			}
		}
	}
	seen := map[string]bool{}
	for i, ch := range c.Checks {
		where := fmt.Sprintf("check[%d]", i)
		if ch.Name != "" {
			where = "check " + ch.Name
		}
		switch {
		case !nameRe.MatchString(ch.Name):
			bad("%s: name must match %s", where, nameRe)
		case seen[ch.Name]:
			bad("%s: duplicate name", where)
		}
		seen[ch.Name] = true
		hasCmd := len(ch.Command) > 0 && ch.Command[0] != ""
		if !hasCmd && ch.Surface == "" || len(ch.Command) > 0 && !hasCmd {
			bad("%s: set a command or surface", where)
		} else if hasCmd && ch.Surface != "" {
			bad("%s: set a command or a surface, not both", where)
		}
		for _, a := range ch.Command {
			if secretFlagRe.MatchString(a) {
				bad("%s: command argument holds a plaintext secret; read it from a credential env var instead", where)
				break
			}
		}
		if strings.TrimSpace(ch.Expect) == "" {
			bad("%s: expect is required", where)
		}
		for _, cn := range ch.Credentials {
			if _, ok := c.Credentials[cn]; !ok {
				bad("%s: credential %q is not defined", where, cn)
			}
		}
	}
	return errs
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
