package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
)

// CheckPath reports whether `saddle` runs by name (#666).
const CheckPath = "saddle on PATH"

// PathReport is what the PATH check reads.
type PathReport struct {
	// Found is where `saddle` resolves on PATH, "" when it doesn't.
	Found string
	// Dir holds the saddle binary that is running: the dir PATH needs.
	Dir string
	// Shell is the login shell's name (bash, zsh, fish, ...).
	Shell string
}

// PathEnv is an Env that can see the user's PATH. The check is skipped for
// envs without it.
type PathEnv interface {
	SaddlePath() PathReport
}

// pathCheck warns, not fails: agents run saddle by absolute path, so only
// the owner's own shell suffers.
func pathCheck(pe PathEnv) Result {
	rep := pe.SaddlePath()
	if rep.Found != "" {
		return withAbout(ok(CheckPath, "saddle resolves to "+rep.Found))
	}
	dir := rep.Dir
	if dir == "" {
		dir = "$(go env GOPATH)/bin"
	}
	return withAbout(warn(CheckPath,
		dir+" is not on PATH, so `saddle` doesn't run by name in a new shell",
		PathFix(rep.Shell, dir)+" (make install in a saddle checkout adds it for you; NO_PATH=1 opts out)"))
}

func withAbout(r Result) Result {
	r.About = "whether typing `saddle` in your shell runs it; agents use its full path either way"
	return r
}

// PathFix is the exact command that puts dir on PATH for shell, matching the
// line scripts/install-path.sh writes.
func PathFix(shell, dir string) string {
	export := `export PATH="$PATH:` + dir + `"`
	switch shell {
	case "bash":
		return "run: echo '" + export + "' >> ~/.bashrc && source ~/.bashrc"
	case "zsh":
		return "run: echo '" + export + "' >> ~/.zshrc && source ~/.zshrc"
	case "fish":
		return "run: fish_add_path " + dir
	}
	return "add this line to your shell's config, then open a new shell: " + export
}

// SaddlePath looks saddle up on this process's PATH.
func (s system) SaddlePath() PathReport {
	var rep PathReport
	if p, err := exec.LookPath("saddle"); err == nil {
		rep.Found = p
	}
	if exe, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		rep.Dir = filepath.Dir(exe)
	}
	rep.Shell = filepath.Base(os.Getenv("SHELL"))
	return rep
}
