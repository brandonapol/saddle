package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/forge"
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

// LocalOnly reports whether saddle can't open PRs: no CLI it can drive the
// repo's provider with.
func (f Forge) LocalOnly() bool { return f.CLI == "" }

// localOnly is what running without a provider CLI turns off.
const localOnly = "saddle runs local-only: no PRs (saddle prs, publish and auto-merge are off) and no merge-settings, " +
	"branch-protection or gh-stack checks; agents, claims and the merge train still work"

// runner is a doctor CLI call as a forge.Runner.
func runner(run func(args ...string) (string, error)) forge.Runner {
	return func(_ context.Context, args ...string) (string, error) { return run(args...) }
}

// setGitLab is the fix when glab works but nothing tells saddle the repo is
// on GitLab.
const setGitLab = `set provider = "gitlab" under [forge] in .saddle/config.toml (and host, for a self-hosted GitLab; see docs/gitlab.md)`

var providerNames = map[string]string{config.ForgeGitHub: "GitHub", config.ForgeGitLab: "GitLab"}

// cliProvider is the provider each CLI drives, and providerCLI the CLI
// each provider is driven with.
var (
	cliProvider = map[string]string{config.ForgeGH: config.ForgeGitHub, config.ForgeGlab: config.ForgeGitLab}
	providerCLI = map[string]string{config.ForgeGitHub: config.ForgeGH, config.ForgeGitLab: config.ForgeGlab}
)

// DetectForge picks the provider from fc ([forge] provider, else the
// provider its cli drives) or remoteURL's host, and checks the CLI for it:
// gh for GitHub, glab for GitLab, logged in for the host (fc.Host when set).
// When neither says, it tries gh and then glab; glab alone isn't enough
// there, since at run time an unknown host means GitHub (forge.New), so
// the fix is to name GitLab in [forge]. "none" runs local-only.
func DetectForge(x Runner, remoteURL string, fc config.Forge) Forge {
	f := Forge{Host: forge.RemoteHost(remoteURL), Provider: fc.Provider}
	if fc.Host != "" {
		f.Host = fc.Host
	}
	if f.Provider == "" {
		f.Provider = cliProvider[fc.CLI]
	}
	if f.Provider == "" {
		f.Provider = forge.HostProvider(f.Host)
	}
	if fc.CLI == config.ForgeNone {
		f.Detail = fmt.Sprintf("%s, local-only ([forge] cli = \"none\"): %s", f.label(), localOnly)
		return f
	}
	order := []string{config.ForgeGH, config.ForgeGlab}
	if c := providerCLI[f.Provider]; c != "" {
		order = []string{c}
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
	switch {
	case f.CLI == "":
		f.Detail = fmt.Sprintf("%s: %s, so %s", f.label(), strings.Join(why, "; "), localOnly)
	case f.Provider == "" && f.CLI == config.ForgeGlab:
		f.Detail = fmt.Sprintf("%s: %s; %s is %s, but nothing says the repo is on GitLab, so %s",
			f.label(), strings.Join(why, "; "), f.Version, f.loggedIn(), localOnly)
		f.Provider, f.CLI, f.Fix = config.ForgeGitLab, "", setGitLab
	default:
		if f.Provider == "" {
			f.Provider = cliProvider[f.CLI]
		}
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
	out, err := x.Exec(cli, forge.AuthStatusArgs(host)...)
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
