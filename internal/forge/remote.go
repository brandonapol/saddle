package forge

import (
	"bufio"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/config"
)

// HostProvider guesses the provider from a remote's host: config.ForgeGitHub,
// config.ForgeGitLab, or "" when the name says neither.
func HostProvider(host string) string {
	switch h := strings.ToLower(host); {
	case strings.Contains(h, "github"):
		return config.ForgeGitHub
	case strings.Contains(h, "gitlab"):
		return config.ForgeGitLab
	}
	return ""
}

// RemoteHost is the host of a git remote URL: scheme://[user@]host[:port]/…
// or the scp-like [user@]host:path. A local path has none.
func RemoteHost(remote string) string {
	remote = strings.TrimSpace(remote)
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Scheme == "file" {
			return ""
		}
		return u.Hostname()
	}
	before, _, found := strings.Cut(remote, ":")
	if !found || strings.Contains(before, "/") {
		return ""
	}
	if _, h, ok := strings.Cut(before, "@"); ok {
		return h
	}
	return before
}

// RepoSlug is the owner/name path of a git remote URL, "" for a local path.
func RepoSlug(remote string) string {
	host := RemoteHost(remote)
	if host == "" {
		return ""
	}
	var path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(strings.TrimSpace(remote))
		if err != nil {
			return ""
		}
		path = u.Path
	} else {
		_, path, _ = strings.Cut(remote, ":")
	}
	path = strings.Trim(path, "/")
	return strings.TrimSuffix(path, ".git")
}

// OriginURL is the URL of the origin remote (else the first remote) of the
// checkout dir is in, read from its git config file so that it starts no
// process. "" when there is none or it can't be read.
func OriginURL(dir string) string {
	cfg := gitConfigPath(dir)
	if cfg == "" {
		return ""
	}
	f, err := os.Open(cfg)
	if err != nil {
		return ""
	}
	defer f.Close()
	var remote, first, origin string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			remote = ""
			if name, ok := strings.CutPrefix(line, "[remote "); ok {
				remote = strings.Trim(strings.TrimSuffix(name, "]"), `"`)
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if remote == "" || !ok || strings.TrimSpace(k) != "url" {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		if remote == "origin" && origin == "" {
			origin = v
		}
		if first == "" {
			first = v
		}
	}
	if origin != "" {
		return origin
	}
	return first
}

// gitConfigPath finds the config file of the repo dir is in, through a
// worktree's or submodule's .git file and commondir.
func gitConfigPath(dir string) string {
	for d := dir; ; {
		gd := filepath.Join(d, ".git")
		fi, err := os.Stat(gd)
		if err == nil {
			if !fi.IsDir() {
				b, err := os.ReadFile(gd)
				if err != nil {
					return ""
				}
				p, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
				if !ok {
					return ""
				}
				gd = strings.TrimSpace(p)
				if !filepath.IsAbs(gd) {
					gd = filepath.Join(d, gd)
				}
			}
			if b, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
				c := strings.TrimSpace(string(b))
				if !filepath.IsAbs(c) {
					c = filepath.Join(gd, c)
				}
				gd = c
			}
			return filepath.Join(gd, "config")
		}
		up := filepath.Dir(d)
		if up == d {
			return ""
		}
		d = up
	}
}
