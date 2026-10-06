package runq

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Gate decides whether the machine has room to start a heavy run right now,
// whatever saddle's own slots say. The head of a class consults it before
// taking a free slot; a closed gate keeps the run queued (at its position)
// for at most Options.GateMaxWait.
type Gate interface {
	Admit() (ok bool, why string)
}

// Load is one sample of machine pressure.
type Load struct {
	Load1  float64 // 1-minute load average
	CPUs   int
	PSI10  float64 // /proc/pressure/cpu "some avg10", percent
	HasPSI bool
}

// LoadSource samples machine pressure.
type LoadSource interface {
	Sample() (Load, error)
}

// ProcLoad reads /proc/loadavg and /proc/pressure/cpu under Root ("/" when
// empty, a fixture directory in tests).
type ProcLoad struct{ Root string }

// Sample reads the load average and, where the kernel has it, CPU PSI.
func (p ProcLoad) Sample() (Load, error) {
	root := p.Root
	if root == "" {
		root = "/"
	}
	l := Load{CPUs: runtime.NumCPU()}
	b, err := os.ReadFile(filepath.Join(root, "proc", "loadavg"))
	if err != nil {
		return l, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return l, fmt.Errorf("runq: empty loadavg")
	}
	if l.Load1, err = strconv.ParseFloat(f[0], 64); err != nil {
		return l, err
	}
	if b, err := os.ReadFile(filepath.Join(root, "proc", "pressure", "cpu")); err == nil {
		l.PSI10, l.HasPSI = parsePSI(string(b))
	}
	return l, nil
}

// parsePSI returns "some avg10" from a /proc/pressure file.
func parsePSI(s string) (float64, bool) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "some" {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "avg10="); ok {
				x, err := strconv.ParseFloat(v, 64)
				return x, err == nil
			}
		}
	}
	return 0, false
}

// LoadGate admits a run while load per CPU and CPU pressure are under their
// ceilings. A zero ceiling disables that check. It fails open: if the source
// can't be read the run starts, because a broken probe must never wedge the
// queue.
type LoadGate struct {
	Source          LoadSource
	MaxLoadPerCPU   float64 // e.g. 0.9
	MaxPSISomeAvg10 float64 // e.g. 60 (percent of time some task waited for CPU)
}

// Admit implements Gate.
func (g LoadGate) Admit() (bool, string) {
	l, err := g.Source.Sample()
	if err != nil {
		return true, ""
	}
	if g.MaxLoadPerCPU > 0 && l.CPUs > 0 {
		if per := l.Load1 / float64(l.CPUs); per > g.MaxLoadPerCPU {
			return false, fmt.Sprintf("load %.2f/cpu > %.2f", per, g.MaxLoadPerCPU)
		}
	}
	if g.MaxPSISomeAvg10 > 0 && l.HasPSI && l.PSI10 > g.MaxPSISomeAvg10 {
		return false, fmt.Sprintf("cpu pressure %.0f%% > %.0f%%", l.PSI10, g.MaxPSISomeAvg10)
	}
	return true, ""
}
