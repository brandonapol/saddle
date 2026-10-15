package tui

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/runq"
)

// Profiles match termenv: 0 is TrueColor, 3 is Ascii (what NO_COLOR selects).
const (
	profileTrue = 0
	profileNone = 3
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func setProfile(t *testing.T, profile int) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	switch profile {
	case profileNone:
		lipgloss.SetColorProfile(profileNone)
	default:
		lipgloss.SetColorProfile(profileTrue)
	}
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })
}

func resourceThresholds() config.TUIResources { return config.Default().TUI.Resources }

// snapOK is a quiet machine: load 4 on 8 cores (0.5 per core), 24 of 32 GB free.
func snapOK() resourceSnap {
	return resourceSnap{Load1: 4, CPUs: 8, MemTotal: 32 << 30, MemAvail: 24 << 30}
}

const (
	goldenWide   = "CPU ▕█████░░░░░▏ load 4.0 / 8 cores  OK to open another\nRAM ▕███░░░░░░░▏ 8.0 / 32 GB free 24  · 2 agents"
	goldenNarrow = "CPU 4.0/8 RAM 8.0/32 OK to open another · 2 agents"
)

func TestResourceVerdict(t *testing.T) {
	th := resourceThresholds()
	cases := []struct {
		per, ram float64
		want     verdict
	}{
		{0.69, 21, verdictOK},
		{0.7, 21, verdictBusy},
		{1.2, 50, verdictBusy},
		{1.21, 50, verdictOver},
		{0.1, 20, verdictBusy},
		{0.1, 20.01, verdictOK},
		{0.1, 10, verdictBusy},
		{0.1, 9.99, verdictOver},
		{2, 5, verdictOver},
		{0.5, 15, verdictBusy},
		{0.5, 75, verdictOK},
	}
	for _, c := range cases {
		if got := judgeResources(c.per, c.ram, th); got != c.want {
			t.Errorf("judge(%.2f load/core, %.2f%% free) = %s, want %s", c.per, c.ram, got, c.want)
		}
	}
	if judgeResources(0.5, 75, th).String() != "OK to open another" ||
		judgeResources(1, 50, th).String() != "Busy: wait" ||
		judgeResources(2, 50, th).String() != "Overloaded: don't" {
		t.Fatal("verdict words changed")
	}
}

// Wide, narrow and NO_COLOR renderings of the same sample are golden.
// Colour is extra: the words match with it stripped.
func TestResourceRenderGolden(t *testing.T) {
	setProfile(t, profileNone)
	th, s := resourceThresholds(), snapOK()
	if got := renderResources(s, th, 2, "", 80, false); got != goldenWide {
		t.Errorf("wide NO_COLOR:\n got %q\nwant %q", got, goldenWide)
	}
	if got := renderResources(s, th, 2, "", 80, true); got != goldenNarrow {
		t.Errorf("narrow NO_COLOR:\n got %q\nwant %q", got, goldenNarrow)
	}
	for _, got := range []string{renderResources(s, th, 2, "", 80, false), renderResources(s, th, 2, "", 40, true)} {
		if strings.Contains(got, "\x1b") {
			t.Errorf("NO_COLOR output has escapes: %q", got)
		}
	}
	// A pane too narrow for two lines collapses even on a wide terminal.
	if got := renderResources(s, th, 2, "", 50, false); got != goldenNarrow {
		t.Errorf("width 50:\n got %q\nwant %q", got, goldenNarrow)
	}
	narrow := renderResources(s, th, 2, "", 80, true)
	if strings.Contains(narrow, "\n") || lipgloss.Width(narrow) > 80 {
		t.Errorf("narrow gauge is not one line: %q", narrow)
	}
	// Very narrow: still one line, cut to the width, verdict kept when it fits.
	cut := renderResources(s, th, 2, "", 44, true)
	if strings.Contains(cut, "\n") || lipgloss.Width(cut) > 44 || !strings.Contains(cut, "OK to open another") {
		t.Errorf("width 44: %q (width %d)", cut, lipgloss.Width(cut))
	}

	setProfile(t, profileTrue)
	colored := renderResources(s, th, 2, "", 80, false)
	if stripANSI(colored) != goldenWide {
		t.Errorf("colored wide:\n got %q\nwant %q", stripANSI(colored), goldenWide)
	}
	if !strings.Contains(colored, fg(cDone)) || strings.Contains(colored, "\x1b") == false {
		t.Errorf("OK verdict should be green: %q", colored)
	}
	busy := s
	busy.Load1 = 8 // 1.0 per core
	if got := renderResources(busy, th, 2, "", 80, false); !strings.Contains(got, fg(cAccent)) || !strings.Contains(stripANSI(got), "Busy: wait") {
		t.Errorf("busy gauge: %q", got)
	}
	hot := s
	hot.Load1 = 16 // 2.0 per core
	if got := renderResources(hot, th, 0, "", 80, false); !strings.Contains(got, fg(cAlert)) || !strings.Contains(stripANSI(got), "Overloaded: don't") || strings.Contains(got, fg(cDone)) {
		t.Errorf("overloaded gauge: %q", got)
	}
	lowRAM := s
	lowRAM.MemAvail = 1 << 30 // about 3%
	if got := stripANSI(renderResources(lowRAM, th, 0, "", 80, true)); !strings.Contains(got, "Overloaded: don't") {
		t.Errorf("low RAM: %q", got)
	}
}

func TestResourceRenderSwapHeavyAndAgents(t *testing.T) {
	setProfile(t, profileNone)
	th := resourceThresholds()
	s := snapOK()
	s.SwapTotal, s.SwapFree = 2<<30, 1<<30
	got := renderResources(s, th, 2, "go-test ×2", 100, false)
	for _, want := range []string{"swap 1.0 GB", "heavy go-test ×2", "2 agents", "8.0 / 32 GB free 24"} {
		if !strings.Contains(got, want) {
			t.Errorf("wide gauge lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(renderResources(snapOK(), th, 2, "", 80, false), "swap") {
		t.Fatal("zero swap was shown")
	}
	one := renderResources(snapOK(), th, 1, "", 80, true)
	if !strings.Contains(one, "1 agent") || strings.Contains(one, "1 agents") {
		t.Errorf("singular: %q", one)
	}
	none := renderResources(snapOK(), th, 0, "", 80, true)
	if !strings.Contains(none, "0 agents") {
		t.Errorf("zero agents: %q", none)
	}
	if renderResources(resourceSnap{}, th, 1, "", 80, false) != "" {
		t.Fatal("an unreadable sample rendered a gauge")
	}
}

func TestHeavyHolderNote(t *testing.T) {
	if heavyHolderNote(nil) != "" {
		t.Fatal("nil queue")
	}
	v := &app.HeavyRuns{Classes: []app.HeavyClass{
		{Class: "go-test", Holders: []app.HeavyEntry{{}, {}}},
		{Class: "lint", Holders: []app.HeavyEntry{{}}},
		{Class: "quiet", Waiters: []app.HeavyEntry{{}}},
	}}
	if got := heavyHolderNote(v); got != "go-test ×2, lint" {
		t.Fatalf("holders = %q", got)
	}
}

func TestResourcesConfigThresholds(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.TUI.Resources
	if !d.Enabled || d.OKLoad != 0.7 || d.BusyLoad != 1.2 || d.OKRAMPct != 20 || d.BusyRAMPct != 10 {
		t.Fatalf("defaults: %+v", d)
	}
	for _, s := range []string{"[tui.resources]", "enabled = true", "ok_load = 0.7", "busy_load = 1.2", "ok_ram_pct = 20", "busy_ram_pct = 10"} {
		if !strings.Contains(config.Template, s) {
			t.Errorf("template lacks %s", s)
		}
	}

	root := t.TempDir()
	writeRepoConfig(t, root, "[tui.resources]\nenabled = false\n")
	cfg, err = config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TUI.Resources.Enabled || cfg.TUI.Resources.OKLoad != 0.7 || cfg.TUI.Resources.BusyRAMPct != 10 {
		t.Fatalf("enabled=false should keep the other defaults: %+v", cfg.TUI.Resources)
	}

	writeRepoConfig(t, root, "[tui.resources]\nenabled = false\nok_load = 0.5\nbusy_load = 0.9\nok_ram_pct = 30\nbusy_ram_pct = 15\n")
	cfg, err = config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.TUI.Resources
	if got.Enabled || got.OKLoad != 0.5 || got.BusyLoad != 0.9 || got.OKRAMPct != 30 || got.BusyRAMPct != 15 {
		t.Fatalf("loaded: %+v", got)
	}

	writeRepoConfig(t, root, "[tui.resources]\nok_load = 2\nbusy_load = 1\n")
	if _, err = config.Load(root); err == nil {
		t.Fatal("busy_load below ok_load was accepted")
	}
	writeRepoConfig(t, root, "[tui.resources]\nok_ram_pct = 10\nbusy_ram_pct = 20\n")
	if _, err = config.Load(root); err == nil {
		t.Fatal("busy_ram_pct above ok_ram_pct was accepted")
	}
	writeRepoConfig(t, root, "[tui.resources]\nok_ram_pct = 150\n")
	if _, err = config.Load(root); err == nil {
		t.Fatal("ok_ram_pct above 100 was accepted")
	}
}

func writeRepoConfig(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, ".saddle")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// View renders the last sample. The sampler runs only when the tick's command
// is executed, and that command is what reads /proc.
func TestViewNeverReadsProc(t *testing.T) {
	setProfile(t, profileNone)
	m := newViewModel(160, 40)
	var calls int
	m.rs.read = func() (resourceSnap, error) {
		calls++
		return snapOK(), nil
	}
	m.rs.have = true
	m.rs.snap = snapOK()
	out := m.View()
	if calls != 0 {
		t.Fatalf("View sampled %d times", calls)
	}
	if !strings.Contains(out, "load 4.0 / 8 cores") || !strings.Contains(out, "OK to open another") {
		t.Fatalf("gauge did not render the injected sample:\n%s", out)
	}
	cmd := m.resourcesTick()
	if cmd == nil {
		t.Fatal("first tick did not schedule a sample")
	}
	msg := cmd()
	if calls != 1 {
		t.Fatalf("tick command sampled %d times", calls)
	}
	m.Update(msg)
	if calls != 1 {
		t.Fatal("applying the sample read again")
	}
	_ = m.View()
	if calls != 1 {
		t.Fatalf("View sampled after the tick: %d", calls)
	}
	// The next two ticks wait; the one after samples again.
	second, third := m.resourcesTick(), m.resourcesTick()
	if second != nil || third != nil {
		t.Fatal("sampled more often than every few seconds")
	}
	cmd = m.resourcesTick()
	if cmd == nil {
		t.Fatal("fourth tick did not sample")
	}
	_ = cmd()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestResourcesGaugeAboveOrchestrator(t *testing.T) {
	setProfile(t, profileNone)
	m := newViewModel(160, 40)
	before := m.View()
	checkScreen(t, "no gauge", before, 160, 40)
	if strings.Contains(before, "OK to open another") {
		t.Fatal("gauge shown before a sample")
	}
	m.rs.have, m.rs.snap = true, snapOK()
	out := m.View()
	checkScreen(t, "gauge", out, 160, 40)
	gi := strings.Index(out, "OK to open another")
	ti := strings.Index(out, "ORCHESTRATOR")
	if gi < 0 || ti < 0 || gi > ti {
		t.Fatalf("gauge at %d, orchestrator box at %d\n%s", gi, ti, out)
	}
	if !strings.Contains(out, "1 agent") {
		t.Fatalf("running agents missing:\n%s", out)
	}

	// A narrow terminal collapses to one line and still sits above the box.
	n := newViewModel(60, 40)
	n.rs.have, n.rs.snap = true, snapOK()
	narrow := n.View()
	checkScreen(t, "narrow gauge", narrow, 60, 40)
	if strings.Contains(narrow, "load 4.0 / 8 cores") || !strings.Contains(narrow, "CPU 4.0/8") {
		t.Fatalf("narrow gauge:\n%s", narrow)
	}
	if strings.Index(narrow, "CPU 4.0/8") > strings.Index(narrow, "ORCHESTRATOR") {
		t.Fatal("narrow gauge is not above the orchestrator box")
	}
}

func TestResourcesDisabledHidesGauge(t *testing.T) {
	m := newViewModel(160, 40)
	m.rs.have, m.rs.snap = true, snapOK()
	m.app.Cfg.TUI.Resources.Enabled = false
	if strings.Contains(m.View(), "OK to open another") {
		t.Fatal("enabled=false still draws the gauge")
	}
	var calls int
	m.rs.read = func() (resourceSnap, error) {
		calls++
		return snapOK(), nil
	}
	for range 5 {
		if m.resourcesTick() != nil {
			t.Fatal("disabled gauge scheduled a sample")
		}
	}
	if calls != 0 {
		t.Fatal("disabled gauge sampled")
	}
}

func TestUnreadableResourcesHideGauge(t *testing.T) {
	m := newViewModel(160, 40)
	m.rs.have, m.rs.snap = true, snapOK()
	m.resourcesLoaded(resourcesMsg{err: errors.New("open /proc/loadavg: no such file")})
	if m.rs.have || strings.Contains(m.View(), "OK to open another") {
		t.Fatal("an unreadable sample stayed on screen")
	}
}

func TestReadProcResourcesUsesRunq(t *testing.T) {
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	if err := os.MkdirAll(proc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "loadavg"), []byte("4.00 1.00 1.00 2/300 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mem := "MemTotal:       33554432 kB\nMemFree:        10000000 kB\nMemAvailable:   25165824 kB\nSwapTotal:       2097152 kB\nSwapFree:        1048576 kB\n"
	if err := os.WriteFile(filepath.Join(proc, "meminfo"), []byte(mem), 0o644); err != nil {
		t.Fatal(err)
	}
	var shells int
	snap, err := readResources(root, func(name string, args ...string) (string, error) {
		shells++
		return "", errors.New("no " + name)
	})
	if err != nil {
		t.Fatal(err)
	}
	if shells != 0 {
		t.Fatalf("/proc was readable but the gauge shelled out %d times", shells)
	}
	want, err := (runq.ProcLoad{Root: root}).Sample()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Load1 != want.Load1 || snap.CPUs != want.CPUs || snap.Load1 != 4 || snap.CPUs != runtime.NumCPU() {
		t.Fatalf("load = %+v, runq = %+v", snap, want)
	}
	if snap.MemTotal != 32<<30 || snap.MemAvail != 24<<30 || snap.swapUsed() != 1<<30 {
		t.Fatalf("mem total %d avail %d swap %d", snap.MemTotal, snap.MemAvail, snap.swapUsed())
	}
	setProfile(t, profileNone)
	// Cores come from the machine, so only the memory line is golden here.
	if got := renderResources(snap, resourceThresholds(), 0, "", 90, false); !strings.Contains(got, "8.0 / 32 GB free 24") || !strings.Contains(got, "swap 1.0 GB") || !strings.Contains(got, "load 4.0 /") {
		t.Fatalf("fixture render:\n%s", got)
	}

	bare := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bare, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bare, "proc", "loadavg"), []byte("1.00 1.00 1.00 1/1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readResources(bare, func(string, ...string) (string, error) {
		return "", errors.New("no proc")
	}); err == nil {
		t.Fatal("missing meminfo was treated as a sample")
	}
	if _, err := parseMeminfo("MemTotal: 1024 kB\n"); err == nil {
		t.Fatal("meminfo without MemAvailable parsed")
	}
}

func TestReadDarwinResources(t *testing.T) {
	vm := `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages free:                               1000000.
Pages active:                             2000000.
Pages inactive:                           500000.
Pages speculative:                        100000.
Pages wired down:                         300000.
`
	run := func(name string, args ...string) (string, error) {
		switch strings.TrimSpace(name + " " + strings.Join(args, " ")) {
		case "sysctl -n vm.loadavg":
			return "{ 2.50 2.00 1.50 }\n", nil
		case "vm_stat":
			return vm, nil
		case "sysctl -n hw.memsize":
			return "17179869184\n", nil
		case "sysctl -n vm.swapusage":
			return "total = 2048.00M  used = 512.00M  free = 1536.00M  (encrypted)\n", nil
		default:
			return "", errors.New("unexpected " + name)
		}
	}
	snap, err := readDarwin(run)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Load1 != 2.5 || snap.CPUs != runtime.NumCPU() {
		t.Fatalf("load %+v", snap)
	}
	// free + inactive + speculative = 1_600_000 pages × 4096.
	if snap.MemAvail != 1600000*4096 || snap.MemTotal != 17179869184 {
		t.Fatalf("mem total %d avail %d", snap.MemTotal, snap.MemAvail)
	}
	if snap.swapUsed() != 512<<20 {
		t.Fatalf("swap used %d", snap.swapUsed())
	}
	if _, err := parseSysctlLoadavg("not a load"); err == nil {
		t.Fatal("garbage loadavg parsed")
	}
	if _, err := parseVMStatAvailable("no pages here"); err == nil {
		t.Fatal("vm_stat without a page size parsed")
	}
	// Swap is optional: a failure there still returns the sample.
	runNoSwap := func(name string, args ...string) (string, error) {
		if strings.Contains(name+" "+strings.Join(args, " "), "swapusage") {
			return "", errors.New("no swap")
		}
		return run(name, args...)
	}
	snap, err = readDarwin(runNoSwap)
	if err != nil || snap.SwapTotal != 0 || snap.Load1 != 2.5 {
		t.Fatalf("swap failure: %+v %v", snap, err)
	}
}
