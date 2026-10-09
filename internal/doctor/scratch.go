package doctor

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/scratch"
)

// CheckScratch reports saddle's scratch root (#322).
const CheckScratch = "scratch"

// ScratchReport is what the scratch check reads.
type ScratchReport struct {
	Root      string
	Bytes     int64
	Files     int
	Space     scratch.Space
	Threshold float64 // percent; 0 means the pressure response is off
	// Strays are saddle's entries in the OS temp dir changed lately: some
	// path still writes saddle's temp files to /tmp.
	Strays []string
	// ProcTmp is an agent's TMPDIR when it isn't under Root.
	ProcTmp string
}

// ScratchEnv is an Env that can measure the scratch root. The check is
// skipped for envs without it.
type ScratchEnv interface {
	Scratch() (ScratchReport, error)
}

func scratchCheck(se ScratchEnv) Result {
	rep, err := se.Scratch()
	if err != nil {
		return warn(CheckScratch, "could not measure: "+err.Error(), "check [tmp] dir in .saddle/config.toml")
	}
	detail := fmt.Sprintf("%s: %s in %d files; %s", rep.Root, scratch.Bytes(rep.Bytes), rep.Files, rep.Space)
	var probs, fixes []string
	if rep.Threshold > 0 && rep.Space.FreePct() < rep.Threshold {
		probs = append(probs, fmt.Sprintf("under %.0f%% free, so spawns are held", rep.Threshold))
		fixes = append(fixes, "saddle gc sweeps saddle's own scratch; free the rest or point [tmp] dir at a bigger disk")
	}
	if len(rep.Strays) > 0 {
		n := min(len(rep.Strays), 5)
		probs = append(probs, fmt.Sprintf("%d recent saddle temp entries in the OS temp dir, so something still writes there: %s",
			len(rep.Strays), strings.Join(rep.Strays[:n], ", ")))
	}
	if rep.ProcTmp != "" {
		probs = append(probs, "this agent's TMPDIR is "+rep.ProcTmp+", outside the scratch root")
	}
	if len(rep.Strays) > 0 || rep.ProcTmp != "" {
		fixes = append(fixes, "export TMPDIR="+rep.Root+" GOTMPDIR="+rep.Root+" for checks you run by hand; saddle gc sweeps the strays once stale")
	}
	if len(probs) == 0 {
		return ok(CheckScratch, detail)
	}
	return warn(CheckScratch, detail+"; "+strings.Join(probs, "; "), strings.Join(fixes, "; "))
}

// Scratch measures the scratch root the way saddle uses it.
func (s system) Scratch() (ScratchReport, error) {
	cfg := scratch.LoadConfig(string(s))
	a := &app.App{Root: string(s)}
	root := a.ScratchRoot()
	rep := ScratchReport{Root: root, Threshold: cfg.LowFree()}
	for _, e := range scratch.Top(root, 1<<30, false, true) {
		rep.Bytes += e.Bytes
		rep.Files += e.Files
	}
	sp, err := scratch.Statfs(root)
	if err != nil {
		return rep, err
	}
	rep.Space = sp
	rep.Strays = scratch.Strays(root, scratch.OSTemp(), cfg.Age(), time.Now())
	// An agent saddle started inherits the root; a shell of the user's need not.
	if t := os.TempDir(); os.Getenv("SADDLE_TASK") != "" && !scratch.Within(root, t) {
		rep.ProcTmp = t
	}
	return rep, nil
}
