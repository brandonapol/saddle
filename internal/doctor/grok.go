package doctor

import (
	"strings"

	"github.com/brandonapol/saddle/internal/config"
)

// CheckGrok is the grok CLI check, run when the harness is grok.
const CheckGrok = "grok"

const grokAbout = "the Grok CLI the agents run in when the harness is grok"

// grok checks the grok CLI is installed and runs, and reports its version.
func (r *run) grok() Result {
	cmd := r.cfg.Grok.Cmd
	if cmd == "" {
		cmd = "grok"
	}
	fix := "install the Grok CLI or set [grok] cmd in .saddle/config.toml"
	prog := program(cmd)
	var res Result
	if _, err := r.env.LookPath(prog); err != nil {
		res = fail(CheckGrok, prog+" not on PATH", fix)
	} else if ver, err := r.env.Exec(prog, "--version"); err != nil {
		res = fail(CheckGrok, prog+" is installed but does not run: "+err.Error(), "run `"+prog+" --version` yourself and fix what it reports, or "+fix)
	} else if ver, _, _ = strings.Cut(ver, "\n"); ver == "" {
		res = warn(CheckGrok, prog+" runs but prints no version", "update the Grok CLI so `"+prog+" --version` prints a version")
	} else {
		res = ok(CheckGrok, ver)
	}
	res.About = grokAbout
	return res
}

func (r *run) wantsGrok() bool { return r.cfg.Harness == config.HarnessGrok }
