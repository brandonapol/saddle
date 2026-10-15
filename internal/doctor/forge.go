package doctor

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/brandonapol/saddle/internal/config"
)

// Runner runs the git provider CLIs. Env is one.
type Runner interface {
	LookPath(name string) (string, error)
	Exec(name string, args ...string) (string, error)
}

// Forge is the git provider a repo's remote is on and the CLI saddle drives
// it with (#646).
type Forge struct {
	Provider string // config.ForgeGitHub, config.ForgeGitLab, or "" when unknown
	Host     string // the remote's host, "" when it has none
	// CLI is the CLI saddle uses, gh or glab, logged in for Host. "" means
	// none is: saddle runs local-only, without PRs.
	CLI     string
	Version string // CLI's --version, first line
	Status  string // CLI's auth status output
	// Detail says which provider and CLI, the auth state and, when saddle
	// runs local-only, what that turns off.
	Detail string
	// Fix is the install or login command that gets a usable CLI, "" when
	// there is nothing to do.
	Fix string
}

// LocalOnly reports whether saddle can't open PRs: no CLI, or one saddle
// doesn't drive yet.
func (f Forge) LocalOnly() bool { return f.CLI != config.ForgeGH }

// localOnly is what running without a provider CLI turns off.
const localOnly = "saddle runs local-only: no PRs (saddle prs, publish and auto-merge are off) and no merge-settings, " +
	"branch-protection or gh-stack checks; agents, claims and the merge train still work"

// gitlabPending is said whenever glab is the CLI, until #644 and #645 land.
const gitlabPending = "GitLab support is not available yet (#644, #645)"

var providerNames = map[string]string{config.ForgeGitHub: "GitHub", config.ForgeGitLab: "GitLab"}

// cliProvider is the provider each CLI drives.
var cliProvider = map[string]string{config.ForgeGH: config.ForgeGitHub, config.ForgeGlab: config.ForgeGitLab}

// DetectForge picks the provider from remoteURL's host (or fc.Provider) and
// the CLI for it: gh for GitHub, glab for GitLab, else the other one when it
// is installed and logged in for the host. fc.CLI names the only CLI to try;
// "none" runs local-only.
func DetectForge(x Runner, remoteURL string, fc config.Forge) Forge {
	f := Forge{Host: remoteHost(remoteURL), Provider: fc.Provider}
	if f.Provider == "" {
		f.Provider = hostProvider(f.Host)
	}
	if fc.CLI == config.ForgeNone {
		f.Detail = fmt.Sprintf("%s, local-only ([forge] cli = \"none\"): %s", f.label(), localOnly)
		return f
	}
	order := []string{config.ForgeGH, config.ForgeGlab}
	switch {
	case fc.CLI != "":
		order = []string{fc.CLI}
	case f.Provider == config.ForgeGitLab:
		order = []string{config.ForgeGlab, config.ForgeGH}
	}
	var why []string
	for i, cli := range order {
		reason, fix := f.try(x, cli)
		if reason == "" {
			break
		}
		why = append(why, reason)
		if i == 0 {
			f.Fix = fix
		}
	}
	if f.Provider == "" && f.CLI != "" {
		f.Provider = cliProvider[f.CLI]
	}
	switch {
	case f.CLI == "":
		f.Detail = fmt.Sprintf("%s: %s, so %s", f.label(), strings.Join(why, "; "), localOnly)
	case f.CLI == config.ForgeGlab && f.Provider == config.ForgeGitLab:
		f.Fix = ""
		f.Detail = fmt.Sprintf("GitLab detected, glab found, %s: %s, %s; %s", gitlabPending, f.Version, f.loggedIn(), localOnly)
	case f.CLI == config.ForgeGlab:
		f.Detail = fmt.Sprintf("%s: %s; %s found, but %s, so %s", f.label(), strings.Join(why, "; "), f.Version, gitlabPending, localOnly)
	default:
		f.Fix = ""
		f.Detail = fmt.Sprintf("%s via %s, %s", f.label(), f.Version, f.loggedIn())
	}
	return f
}

// try makes cli f's CLI when it is installed and logged in for f's host, and
// otherwise returns why not and the fix.
func (f *Forge) try(x Runner, cli string) (reason, fix string) {
	host := f.Host
	login := cli + " auth login"
	if host != "" {
		login += " --hostname " + host
	}
	if _, err := x.LookPath(cli); err != nil {
		install := "install the GitHub CLI (https://cli.github.com)"
		if cli == config.ForgeGlab {
			install = "install the GitLab CLI (https://gitlab.com/gitlab-org/cli)"
		}
		return cli + " is not installed", install + ", then " + login
	}
	args := []string{"auth", "status"}
	if host != "" {
		args = append(args, "--hostname", host)
	}
	out, err := x.Exec(cli, args...)
	if err != nil {
		where := ""
		if host != "" {
			where = " to " + host
		}
		return cli + " is not logged in" + where, login
	}
	ver, _ := x.Exec(cli, "--version")
	if ver, _, _ = strings.Cut(ver, "\n"); ver == "" {
		ver = cli
	}
	f.CLI, f.Version, f.Status = cli, ver, out
	return "", ""
}

func (f Forge) label() string {
	name := providerNames[f.Provider]
	switch {
	case name == "" && f.Host == "":
		return "unknown provider (no remote host)"
	case name == "":
		return "unknown provider (" + f.Host + ")"
	case f.Host == "":
		return name
	}
	return name + " (" + f.Host + ")"
}

func (f Forge) loggedIn() string {
	if f.Host == "" {
		return "logged in"
	}
	return "logged in to " + f.Host
}

// hostProvider guesses the provider from a remote's host.
func hostProvider(host string) string {
	switch h := strings.ToLower(host); {
	case strings.Contains(h, "github"):
		return config.ForgeGitHub
	case strings.Contains(h, "gitlab"):
		return config.ForgeGitLab
	}
	return ""
}

// remoteHost is the host of a git remote URL: scheme://[user@]host[:port]/…
// or the scp-like [user@]host:path. A local path has none.
func remoteHost(remote string) string {
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
