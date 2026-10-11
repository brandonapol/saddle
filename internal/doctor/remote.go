package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/remote"
)

// CheckRemoteControl reports what remote control exposes (#315).
const CheckRemoteControl = "remote control"

// remoteLongTTL is the token life past which the check warns. Tokens are
// short-lived by design: a day by default, a month at most.
const remoteLongTTL = 7 * 24 * time.Hour

// RemoteReport is what the remote-control check reads.
type RemoteReport struct {
	Config    remote.Config
	ConfigErr error
	Dir       string // where tokens and the audit log live
	Tokens    []remote.Token
	TokensErr error
	// Loose are the remote dir and files in it that others can read or
	// write, each as "path (mode …)".
	Loose []string
}

// RemoteEnv is an Env that can read the user's remote-control setup. The
// check is skipped for envs without it.
type RemoteEnv interface {
	Remote() (RemoteReport, error)
}

func (s system) Remote() (RemoteReport, error) {
	dir, err := remote.Dir()
	if err != nil {
		return RemoteReport{}, err
	}
	return ReadRemote(remote.UserConfigPath(), dir), nil
}

// ReadRemote reads the [remote] config at cfgPath and the tokens, audit
// logs and their modes in dir.
func ReadRemote(cfgPath, dir string) RemoteReport {
	rep := RemoteReport{Dir: dir}
	rep.Config, rep.ConfigErr = remote.LoadConfig(cfgPath)
	if fi, err := os.Stat(dir); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			rep.TokensErr = err
		}
		return rep
	} else if fi.Mode().Perm()&0o077 != 0 {
		rep.Loose = append(rep.Loose, fmt.Sprintf("%s (mode %v)", dir, fi.Mode().Perm()))
	}
	files, _ := filepath.Glob(filepath.Join(dir, "audit.log*"))
	files = append([]string{filepath.Join(dir, "tokens.json")}, files...)
	for _, p := range files {
		if fi, err := os.Stat(p); err == nil && fi.Mode().Perm()&0o077 != 0 {
			rep.Loose = append(rep.Loose, fmt.Sprintf("%s (mode %v)", p, fi.Mode().Perm()))
		}
	}
	rep.Tokens, rep.TokensErr = remote.NewTokens(dir).List()
	return rep
}

func remoteCheck(re RemoteEnv, now time.Time) Result {
	rep, err := re.Remote()
	if err != nil {
		return warn(CheckRemoteControl, "could not read: "+err.Error(), "set "+remote.EnvDir+" or check ~/.config/saddle/remote")
	}
	const fixConfig = "fix [remote] in ~/.config/saddle/config.toml (see docs/remote-control.md)"
	if rep.ConfigErr != nil {
		return fail(CheckRemoteControl, "config: "+rep.ConfigErr.Error(), fixConfig)
	}
	cfg := rep.Config
	detail := "off"
	if cfg.Enabled {
		if err := cfg.Check(); err != nil {
			return fail(CheckRemoteControl, "on, but serving is refused: "+err.Error(), fixConfig)
		}
		where, transport := "loopback", "plain HTTP"
		if remote.CheckListen(cfg.Listen) != nil {
			where = "public"
		}
		if cfg.TLS() {
			transport = "TLS"
		}
		detail = fmt.Sprintf("on, listening on %s (%s, %s)", cfg.Listen, where, transport)
	}
	var live, long []string
	for _, t := range rep.Tokens {
		if t.Revoked != nil || !now.Before(t.Expires) {
			continue
		}
		scopes := make([]string, len(t.Scopes))
		for i, s := range t.Scopes {
			scopes[i] = string(s)
		}
		desc := fmt.Sprintf("%s (%s, expires %s", t.Name, strings.Join(scopes, ","), t.Expires.Local().Format("2006-01-02 15:04"))
		if len(t.Repos) > 0 {
			desc += ", repos " + strings.Join(t.Repos, ",")
		}
		live = append(live, desc+")")
		if life := t.Expires.Sub(t.Created); life > remoteLongTTL {
			long = append(long, fmt.Sprintf("%s lives %dd", t.Name, int(life.Hours()/24)))
		}
	}
	switch len(live) {
	case 0:
		detail += "; no live tokens"
	case 1:
		detail += "; 1 live token: " + live[0]
	default:
		detail += fmt.Sprintf("; %d live tokens: %s", len(live), strings.Join(live, "; "))
	}
	var probs, fixes []string
	if rep.TokensErr != nil {
		probs = append(probs, "tokens: "+rep.TokensErr.Error())
	}
	if len(rep.Loose) > 0 {
		probs = append(probs, "others can read "+strings.Join(rep.Loose, ", "))
		fixes = append(fixes, fmt.Sprintf("chmod 700 %s and chmod 600 the files in it", rep.Dir))
	}
	if len(long) > 0 {
		probs = append(probs, "long-lived tokens ("+strings.Join(long, ", ")+")")
		for _, t := range long {
			name, _, _ := strings.Cut(t, " ")
			fixes = append(fixes, "saddle remote token revoke "+name)
		}
		fixes[len(fixes)-1] += ", then create a shorter-lived one (--ttl, a day by default)"
	}
	if len(probs) == 0 {
		return ok(CheckRemoteControl, detail)
	}
	return warn(CheckRemoteControl, detail+"; "+strings.Join(probs, "; "), strings.Join(fixes, "; "))
}
