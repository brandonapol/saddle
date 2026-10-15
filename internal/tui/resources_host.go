package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/brandonapol/saddle/internal/runq"
)

// readHostResources samples this machine. Load comes from runq.ProcLoad, the
// same /proc/loadavg reader the heavy-run gate uses. Memory comes from
// /proc/meminfo. On macOS, where /proc is missing, it falls back to
// sysctl vm.loadavg, vm_stat and vm.swapusage. An error hides the gauge.
func readHostResources() (resourceSnap, error) {
	return readResources("/", execText)
}

// commandFunc runs a host command and returns its stdout.
type commandFunc func(name string, args ...string) (string, error)

func execText(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// readResources reads load and memory under root ("/" on the host, a fixture
// in tests). run is the macOS fallback and is not called when /proc answers.
func readResources(root string, run commandFunc) (resourceSnap, error) {
	if root == "" {
		root = "/"
	}
	load, lerr := (runq.ProcLoad{Root: root}).Sample()
	mem, merr := readMeminfoFile(filepath.Join(root, "proc", "meminfo"))
	if lerr == nil && merr == nil {
		return resourceSnap{
			Load1:     load.Load1,
			CPUs:      load.CPUs,
			MemTotal:  mem.total,
			MemAvail:  mem.avail,
			SwapTotal: mem.swapTotal,
			SwapFree:  mem.swapFree,
		}, nil
	}
	if runtime.GOOS == "darwin" && run != nil {
		if snap, err := readDarwin(run); err == nil {
			return snap, nil
		}
	}
	if lerr != nil {
		return resourceSnap{}, lerr
	}
	return resourceSnap{}, merr
}

type memInfo struct {
	total, avail        uint64
	swapTotal, swapFree uint64
}

func readMeminfoFile(path string) (memInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return memInfo{}, err
	}
	return parseMeminfo(string(b))
}

// parseMeminfo reads MemTotal, MemAvailable and swap from /proc/meminfo.
// Values are bytes. Missing swap is zero, not an error.
func parseMeminfo(s string) (memInfo, error) {
	var info memInfo
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(s, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		b := kb * 1024
		switch key {
		case "MemTotal":
			info.total, haveTotal = b, true
		case "MemAvailable":
			info.avail, haveAvail = b, true
		case "SwapTotal":
			info.swapTotal = b
		case "SwapFree":
			info.swapFree = b
		}
	}
	if !haveTotal || !haveAvail || info.total == 0 {
		return memInfo{}, fmt.Errorf("resources: meminfo missing MemTotal or MemAvailable")
	}
	if info.swapFree > info.swapTotal {
		info.swapFree = info.swapTotal
	}
	return info, nil
}

// readDarwin samples a Mac: sysctl vm.loadavg for the 1-minute load, vm_stat
// for memory that can be given to new work, hw.memsize for the total, and
// vm.swapusage for swap. A missing swap reading leaves swap at zero.
func readDarwin(run commandFunc) (resourceSnap, error) {
	loadText, err := run("sysctl", "-n", "vm.loadavg")
	if err != nil {
		return resourceSnap{}, err
	}
	load, err := parseSysctlLoadavg(loadText)
	if err != nil {
		return resourceSnap{}, err
	}
	vmText, err := run("vm_stat")
	if err != nil {
		return resourceSnap{}, err
	}
	avail, err := parseVMStatAvailable(vmText)
	if err != nil {
		return resourceSnap{}, err
	}
	memText, err := run("sysctl", "-n", "hw.memsize")
	if err != nil {
		return resourceSnap{}, err
	}
	total, err := parseMemsize(memText)
	if err != nil {
		return resourceSnap{}, err
	}
	if total == 0 {
		return resourceSnap{}, fmt.Errorf("resources: hw.memsize is 0")
	}
	snap := resourceSnap{Load1: load, CPUs: runtime.NumCPU(), MemTotal: total, MemAvail: avail}
	if swapText, err := run("sysctl", "-n", "vm.swapusage"); err == nil {
		snap.SwapTotal, snap.SwapFree = parseSwapUsage(swapText)
	}
	return snap, nil
}

// parseSysctlLoadavg reads the 1-minute load from `sysctl -n vm.loadavg`,
// which looks like "{ 1.94 1.83 1.76 }".
func parseSysctlLoadavg(s string) (float64, error) {
	s = strings.Trim(s, "{}\n\t ")
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, fmt.Errorf("resources: empty vm.loadavg")
	}
	n, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("resources: vm.loadavg %q: %w", f[0], err)
	}
	return n, nil
}

func parseMemsize(s string) (uint64, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, fmt.Errorf("resources: empty hw.memsize")
	}
	n, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("resources: hw.memsize %q: %w", f[0], err)
	}
	return n, nil
}

// parseVMStatAvailable is the macOS stand-in for MemAvailable: free,
// inactive and speculative pages times the page size vm_stat reports.
func parseVMStatAvailable(s string) (uint64, error) {
	const mark = "page size of "
	i := strings.Index(s, mark)
	if i < 0 {
		return 0, fmt.Errorf("resources: vm_stat has no page size")
	}
	pageField := strings.Fields(s[i+len(mark):])
	if len(pageField) == 0 {
		return 0, fmt.Errorf("resources: vm_stat has no page size")
	}
	page, err := strconv.ParseUint(pageField[0], 10, 64)
	if err != nil || page == 0 {
		return 0, fmt.Errorf("resources: vm_stat page size %q", pageField[0])
	}
	pages := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		num := strings.Trim(strings.TrimSpace(rest), ".")
		if num == "" {
			continue
		}
		n, err := strconv.ParseUint(strings.Fields(num)[0], 10, 64)
		if err != nil {
			continue
		}
		pages[strings.TrimSpace(key)] = n
	}
	free, ok := pages["Pages free"]
	if !ok {
		return 0, fmt.Errorf("resources: vm_stat has no free pages")
	}
	return (free + pages["Pages inactive"] + pages["Pages speculative"]) * page, nil
}

// parseSwapUsage reads `sysctl -n vm.swapusage`:
// "total = 2048.00M  used = 512.00M  free = 1536.00M  (encrypted)".
func parseSwapUsage(s string) (total, free uint64) {
	fields := strings.Fields(s)
	for i := 0; i+2 < len(fields); i++ {
		if fields[i+1] != "=" {
			continue
		}
		b := parseSizeToken(fields[i+2])
		switch fields[i] {
		case "total":
			total = b
		case "free":
			free = b
		}
	}
	if free > total {
		free = total
	}
	return total, free
}

func parseSizeToken(s string) uint64 {
	s = strings.TrimRight(s, ".,)")
	mult := float64(1)
	if s != "" {
		switch s[len(s)-1] {
		case 'K', 'k':
			mult = 1 << 10
			s = s[:len(s)-1]
		case 'M', 'm':
			mult = 1 << 20
			s = s[:len(s)-1]
		case 'G', 'g':
			mult = 1 << 30
			s = s[:len(s)-1]
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return uint64(f * mult)
}
