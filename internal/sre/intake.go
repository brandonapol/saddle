package sre

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/secretscan"
)

// Intake is pasted incident text and what intake pulled out of it.
type Intake struct {
	Text    string
	Summary string   // the first non-empty line, shortened
	Paths   []string // source paths as written (stack frames, file:line), not yet resolved
}

// pathRe finds file paths with a source extension, optionally followed by
// :line. Hosts such as api.example.com do not match: the extension list is
// closed.
var pathRe = regexp.MustCompile(`(?:[\w.@-]*/)*[\w.-]+\.(?:go|py|js|mjs|ts|tsx|jsx|rb|java|kt|rs|c|h|cc|cpp|cs|php|swift|scala|sql|ya?ml|toml|json|tf|sh|proto)\b`)

// ParseIncident pulls the summary and the mentioned source paths out of text.
func ParseIncident(text string) Intake {
	inc := Intake{Text: strings.TrimSpace(text)}
	for _, l := range strings.Split(inc.Text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			inc.Summary = shorten(l, 60)
			break
		}
	}
	seen := map[string]bool{}
	for _, p := range pathRe.FindAllString(inc.Text, -1) {
		if !seen[p] {
			seen[p] = true
			inc.Paths = append(inc.Paths, p)
		}
	}
	return inc
}

func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n])) + "…"
	}
	return s
}

// Check statuses.
const (
	CheckOK      = "ok"      // the command exited 0; compare its output with expect
	CheckFailed  = "failed"  // the command exited non-zero or did not run
	CheckSkipped = "skipped" // a credential env var is unset
	CheckManual  = "manual"  // a surface: an investigator looks at it
)

// CheckResult is the outcome of one preemptive check.
type CheckResult struct {
	Name   string
	Status string
	Expect string
	Output string // redacted and truncated
	Note   string
}

// Runner runs one read-only check command in dir and returns its output.
type Runner interface {
	Run(ctx context.Context, dir string, argv []string) (string, error)
}

const (
	maxOutput    = 2000
	truncMark    = "\n…(truncated)"
	checkTimeout = 30 * time.Second
)

// ExecRunner runs checks as local processes with a timeout.
type ExecRunner struct{}

// Run runs argv in dir and returns its combined output, truncated.
func (ExecRunner) Run(ctx context.Context, dir string, argv []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	out := b.String()
	if len(out) > maxOutput {
		out = out[:maxOutput] + truncMark
	}
	return out, err
}

// RunChecks runs cfg's preemptive checks. Commands are the config's
// read-only argv; a check whose credential env var is unset is skipped, and a
// surface check is left for an investigator. Output is redacted.
func RunChecks(ctx context.Context, cfg *Config, dir string, r Runner) []CheckResult {
	if cfg == nil {
		return nil
	}
	out := make([]CheckResult, 0, len(cfg.Checks))
	for _, ch := range cfg.Checks {
		res := CheckResult{Name: ch.Name, Expect: ch.Expect}
		switch {
		case len(ch.Command) == 0:
			res.Status = CheckManual
			res.Note = "look at " + describeSurface(cfg, ch.Surface)
		case missingEnv(cfg, ch) != "":
			res.Status = CheckSkipped
			res.Note = "credential env " + missingEnv(cfg, ch) + " is not set"
		default:
			o, err := r.Run(ctx, dir, ch.Command)
			res.Output = redact(strings.TrimSpace(o))
			res.Status = CheckOK
			if err != nil {
				res.Status = CheckFailed
				res.Note = redact(err.Error())
			}
		}
		out = append(out, res)
	}
	return out
}

func describeSurface(cfg *Config, s string) string {
	if name, ok := strings.CutPrefix(s, "endpoint:"); ok {
		if e, ok := cfg.Endpoints[name]; ok {
			return fmt.Sprintf("%s (%s)", s, e.URL)
		}
	}
	return s
}

func missingEnv(cfg *Config, ch Check) string {
	for _, n := range ch.Credentials {
		if e := cfg.Credentials[n].Env; e != "" && os.Getenv(e) == "" {
			return e
		}
	}
	return ""
}

func redact(s string) string {
	for _, m := range secretscan.Find([]byte(s)) {
		s = strings.ReplaceAll(s, string(m), "[redacted]")
	}
	return s
}
