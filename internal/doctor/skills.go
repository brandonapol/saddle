package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/orch"
)

// CheckSkills reports the slash commands and skills the orchestrator sees.
const CheckSkills = "orchestrator skills"

// SkillsEnv is an Env that can ask a short headless Claude session which
// commands it offers (#255). The probe starts a session, so saddle up's
// quick preflight uses an Env without it and skips the check.
type SkillsEnv interface {
	// OrchestratorCommands starts claude the way the orchestrator runs
	// (stream-json, in Root), lists its commands and stops it. No model
	// call is made.
	OrchestratorCommands(cfg config.Config) ([]orch.Command, error)
	// DiskSkills maps each skills directory Claude Code reads (user and
	// project) to the skills in it, for those that have any.
	DiskSkills() map[string][]string
}

func (r *run) skills(se SkillsEnv) Result {
	disk := se.DiskSkills()
	dirs := make([]string, 0, len(disk))
	for d := range disk {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	cs, err := se.OrchestratorCommands(r.cfg)
	if err != nil {
		return warn(CheckSkills, "could not list the orchestrator's commands: "+firstLine(err.Error()),
			"run `"+r.cfg.Claude.Cmd+"` in the repo and type / to see them; check it starts and is logged in")
	}
	fix := "check nothing turns them off for saddle: --bare or --disable-slash-commands in [claude] cmd, " +
		"a CLAUDE_CONFIG_DIR that points elsewhere, or a skill's SKILL.md missing its frontmatter"
	if len(cs) == 0 {
		if len(dirs) == 0 {
			return ok(CheckSkills, "no skills or slash commands")
		}
		return warn(CheckSkills, "the orchestrator sees no skills or slash commands, but "+strings.Join(dirs, " and ")+" hold skills", fix)
	}
	seen := map[string]bool{}
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		seen[c.Name] = true
		names = append(names, "/"+c.Name)
	}
	var missing, where []string
	for _, d := range dirs {
		before := len(missing)
		for _, s := range disk[d] {
			if !seen[s] {
				missing = append(missing, s)
			}
		}
		if len(missing) > before {
			where = append(where, d)
		}
	}
	if len(missing) > 0 {
		return warn(CheckSkills, fmt.Sprintf("the orchestrator doesn't see %s from %s", strings.Join(missing, ", "), strings.Join(where, " and ")), fix)
	}
	const show = 15
	list := strings.Join(names[:min(show, len(names))], " ")
	if len(names) > show {
		list += fmt.Sprintf(" … (%d more)", len(names)-show)
	}
	return ok(CheckSkills, fmt.Sprintf("%d slash commands: %s", len(cs), list))
}

// WithSkillsProbe is System plus the orchestrator skills probe, for saddle
// doctor.
func WithSkillsProbe(root string) Env { return probing{system(root)} }

type probing struct{ system }

// probeTimeout bounds the skills probe; a session normally answers in a
// few seconds.
var probeTimeout = 20 * time.Second

func (p probing) OrchestratorCommands(cfg config.Config) ([]orch.Command, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.Command(cfg.Claude.Cmd, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
	cmd.Dir = p.Root()
	cs, err := orch.Probe(ctx, cmd)
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("no answer in %s", probeTimeout)
	}
	return cs, err
}

func (p probing) DiskSkills() map[string][]string {
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cfg = filepath.Join(home, ".claude")
		}
	}
	out := map[string][]string{}
	for _, dir := range []string{filepath.Join(cfg, "skills"), filepath.Join(p.Root(), ".claude", "skills")} {
		ms, _ := filepath.Glob(filepath.Join(dir, "*", "SKILL.md"))
		for _, m := range ms {
			out[dir] = append(out[dir], filepath.Base(filepath.Dir(m)))
		}
		slices.Sort(out[dir])
	}
	for d, s := range out {
		if len(s) == 0 {
			delete(out, d)
		}
	}
	return out
}
