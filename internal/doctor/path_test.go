package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// saddlePathEnv adds a PATH report to the fake.
type saddlePathEnv struct {
	*fakeEnv
	rep PathReport
}

func (e saddlePathEnv) SaddlePath() PathReport { return e.rep }

func pathResult(t *testing.T, env Env) (Result, bool) {
	t.Helper()
	for _, r := range Run(env) {
		if r.Name == CheckPath {
			return r, true
		}
	}
	return Result{}, false
}

func TestPathCheck(t *testing.T) {
	base := healthy(t)
	for _, tc := range []struct {
		name   string
		rep    PathReport
		status Status
		detail string
		fix    string
	}{
		{"on PATH", PathReport{Found: "/home/u/go/bin/saddle", Dir: "/home/u/go/bin", Shell: "bash"}, OK, "/home/u/go/bin/saddle", ""},
		{"bash", PathReport{Dir: "/home/u/go/bin", Shell: "bash"}, Warn, "/home/u/go/bin is not on PATH",
			`run: echo 'export PATH="$PATH:/home/u/go/bin"' >> ~/.bashrc && source ~/.bashrc`},
		{"zsh", PathReport{Dir: "/home/u/go/bin", Shell: "zsh"}, Warn, "not on PATH",
			`run: echo 'export PATH="$PATH:/home/u/go/bin"' >> ~/.zshrc && source ~/.zshrc`},
		{"fish", PathReport{Dir: "/home/u/go/bin", Shell: "fish"}, Warn, "not on PATH", "run: fish_add_path /home/u/go/bin"},
		{"other shell", PathReport{Dir: "/opt/bin", Shell: "tcsh"}, Warn, "not on PATH", `export PATH="$PATH:/opt/bin"`},
		{"no dir", PathReport{Shell: "bash"}, Warn, "$(go env GOPATH)/bin", "$(go env GOPATH)/bin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, found := pathResult(t, saddlePathEnv{fakeEnv: base, rep: tc.rep})
			if !found {
				t.Fatal("no PATH check")
			}
			if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) || !strings.Contains(r.Fix, tc.fix) {
				t.Fatalf("%+v", r)
			}
			if r.About == "" {
				t.Error("no about text")
			}
		})
	}
}

func TestPathCheckSkippedWithoutPathEnv(t *testing.T) {
	if _, found := pathResult(t, healthy(t)); found {
		t.Fatal("PATH check ran for an env that can't see PATH")
	}
}

// TestPathFixMatchesInstallScript runs scripts/install-path.sh against a temp
// HOME and checks doctor's fix names the very line it writes.
func TestPathFixMatchesInstallScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	script, err := filepath.Abs("../../scripts/install-path.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := "/opt/saddle bin"
	for _, sh := range []struct{ name, rc string }{
		{"bash", ".bashrc"}, {"zsh", ".zshrc"}, {"fish", ".config/fish/config.fish"},
	} {
		t.Run(sh.name, func(t *testing.T) {
			home := t.TempDir()
			cmd := exec.Command("bash", script, dir)
			cmd.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
				"SHELL=/bin/" + sh.name, "PATH=" + os.Getenv("PATH")}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			b, err := os.ReadFile(filepath.Join(home, sh.rc))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			written := lines[len(lines)-1]
			if fix := PathFix(sh.name, dir); !strings.Contains(fix, written) {
				t.Fatalf("script wrote %q; doctor's fix is %q", written, fix)
			}
		})
	}
}
