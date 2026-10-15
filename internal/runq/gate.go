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
	// MemTotalKB and MemAvailKB are /proc/meminfo's MemTotal and
	// MemAvailable, the same reading as the TUI gauge (#600).
	MemTotalKB, MemAvailKB uint64
	HasMem                 bool
}

// FreeRAMPct is available RAM as a percent of total.
func (l Load) FreeRAMPct() float64 {
	if l.MemTotalKB == 0 {
		return 100
	}
	return float64(l.MemAvailKB) / float64(l.MemTotalKB) * 100
}

// LoadSource samples machine pressure.
type LoadSource interface {
	Sample() (Load, error)
}

// ProcLoad reads /proc/loadavg, /proc/pressure/cpu and /proc/meminfo under Root ("/" when
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
	if b, err := os.ReadFile(filepath.Join(root, "proc", "meminfo")); err == nil {
		l.MemTotalKB, l.MemAvailKB, l.HasMem = parseMeminfo(string(b))
	}
	return l, nil
}

// parseMeminfo returns MemTotal and MemAvailable in kB.
func parseMeminfo(s string) (total, avail uint64, ok bool) {
	var haveT, haveA bool
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total, haveT = v, true
		case "MemAvailable:":
			avail, haveA = v, true
		}
	}
	return total, avail, haveT && haveA && total > 0
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
// ceilings and free RAM is over its floor. A zero limit disables that check. It fails open: if the source
// can't be read the run starts, because a broken probe must never wedge the
// queue.
type LoadGate struct {
	Source          LoadSource
	MaxLoadPerCPU   float64 // e.g. 0.9
	MaxPSISomeAvg10 float64 // e.g. 60 (percent of time some task waited for CPU)
	MinFreeRAMPct   float64 // e.g. 10 (MemAvailable as a percent of MemTotal)
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
	if g.MinFreeRAMPct > 0 && l.HasMem {
		if free := l.FreeRAMPct(); free < g.MinFreeRAMPct {
			return false, fmt.Sprintf("free ram %.0f%% < %.0f%%", free, g.MinFreeRAMPct)
		}
	}
	return true, ""
}
