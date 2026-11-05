package gitx

import (
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/netx"
)

// ErrNetTimeout is what a network git call that ran past its deadline
// returns (see RunCtx). It is netx.ErrNetTimeout.
var ErrNetTimeout = netx.ErrNetTimeout

// NetTimeout is the deadline every network git call gets, so one push or
// fetch on a connection that died silently can't hold train.lock for hours
// (#788). SADDLE_GIT_NET_TIMEOUT (a Go duration, e.g. "10m") overrides the
// 3 minute default; 0 here turns the deadline off.
var NetTimeout = netx.Duration("SADDLE_GIT_NET_TIMEOUT", 3*time.Minute)

// sshKeepalive makes ssh notice a dead connection within a minute: four
// unanswered keepalives 15s apart.
const sshKeepalive = " -o ServerAliveInterval=15 -o ServerAliveCountMax=4"

// IsNetwork reports whether git args (after -C dir) run a subcommand that
// talks to a remote.
func IsNetwork(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "-C" || a == "--git-dir" || a == "--work-tree":
			i++ // the option's value
		case strings.HasPrefix(a, "-"):
		default:
			switch a {
			case "push", "fetch", "pull", "ls-remote", "clone":
				return true
			}
			return false
		}
	}
	return false
}

// netEnv is the environment a network git call adds: HTTP transfers that
// move under 1000 bytes/s for 60s abort, and ssh sends keepalives. A user's
// own settings win: the low-speed variables are only set when unset, and the
// keepalives go on the end of the ssh command git would use anyway. A GIT_SSH
// program is left alone, since options can't be added to it.
func netEnv(dir string) []string {
	var env []string
	if os.Getenv("GIT_HTTP_LOW_SPEED_LIMIT") == "" {
		env = append(env, "GIT_HTTP_LOW_SPEED_LIMIT=1000")
	}
	if os.Getenv("GIT_HTTP_LOW_SPEED_TIME") == "" {
		env = append(env, "GIT_HTTP_LOW_SPEED_TIME=60")
	}
	ssh := os.Getenv("GIT_SSH_COMMAND")
	if ssh == "" {
		if os.Getenv("GIT_SSH") != "" {
			return env
		}
		ssh = sshCommand(dir)
	}
	if !strings.Contains(ssh, "ServerAlive") {
		env = append(env, "GIT_SSH_COMMAND="+ssh+sshKeepalive)
	}
	return env
}

// sshCommand is the repo's core.sshCommand, or plain ssh.
func sshCommand(dir string) string {
	cmd := exec.Command("git", "-C", dir, "config", "--get", "core.sshCommand")
	cmd.Env = Env()
	if out, err := cmd.Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	return "ssh"
}
