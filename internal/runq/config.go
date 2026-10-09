package runq

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is a runq.toml file. Saddle reads the repo's .saddle/runq.toml for
// defaults, then the user's ~/.config/saddle/runq.toml, whose values win:
// slot counts are a property of the machine (docs/runq.md Q7).
//
//	mode = "observe"        # off | observe | enforce
//	heartbeat = "2s"
//	stale_after = "15s"
//	aging_step = "30s"
//	gate_max_wait = "10m"
//	wait_max = "30m"         # saddle run --wait-max default
//	max_run = "30m"          # a holder running longer shows as overdue
//	default_slots = 1        # for classes named nowhere
//	max_load_per_cpu = 1.0   # load gate: hold while load1/cores is over this (0 off)
//	max_cpu_pressure = 60    # ... or CPU PSI "some avg10" is over this % (0 off)
//	nice = 10                # heavy children run at this nice increment (0 off)
//	ionice = true            # ... and idle I/O class, on Linux
//	scope = "systemd"        # run them in a user systemd scope (off by default)
//	cpu_weight = 20          # the scope's CPUWeight
//	cpu_quota = "400%"       # an optional hard ceiling (CPUQuota)
//	adaptive_slots = true    # size slots from history daily (off by default)
//	target_util = 0.75       # share of the cores adaptive slots aim to fill
//	[classes.go-test]
//	slots = 2
//	match = ["go test*", "make check"]
//	max_run = "45m"          # this class's own max_run
type Config struct {
	Mode         Mode                   `toml:"mode"`
	Heartbeat    Duration               `toml:"heartbeat"`
	StaleAfter   Duration               `toml:"stale_after"`
	AgingStep    Duration               `toml:"aging_step"`
	GateMaxWait  Duration               `toml:"gate_max_wait"`
	WaitMax      Duration               `toml:"wait_max"`
	MaxRun       Duration               `toml:"max_run"`
	DefaultSlots int                    `toml:"default_slots"`
	Classes      map[string]ClassConfig `toml:"classes"`

	// The load gate (docs/runq.md Q3). Nil means the default; 0 turns that
	// check off.
	MaxLoadPerCPU  *float64 `toml:"max_load_per_cpu"`
	MaxCPUPressure *float64 `toml:"max_cpu_pressure"`

	// How heavy children run. Nil means the default (nice 10, idle I/O).
	Nice   *int   `toml:"nice"`
	IONice *bool  `toml:"ionice"`
	Scope  string `toml:"scope"` // "" or "none": no scope; "systemd"
	// CPUWeight is the scope's weight (default 20); CPUQuota an optional
	// hard ceiling such as "400%".
	CPUWeight int    `toml:"cpu_weight"`
	CPUQuota  string `toml:"cpu_quota"`

	// AdaptiveSlots sizes slots from a week of history once a day.
	AdaptiveSlots *bool   `toml:"adaptive_slots"`
	TargetUtil    float64 `toml:"target_util"`
}

// Defaults for the load gate and heavy children.
const (
	DefaultMaxLoadPerCPU  = 1.0
	DefaultMaxCPUPressure = 60
	DefaultNice           = 10
	DefaultCPUWeight      = 20
	DefaultTargetUtil     = 0.75
)

// ClassConfig is one [classes.<name>] table. Match holds the argv patterns
// the hook and shims (#240) route to the class; the core doesn't read them.
type ClassConfig struct {
	Slots  int      `toml:"slots"`
	Match  []string `toml:"match"`
	MaxRun Duration `toml:"max_run"`
}

// DefaultMaxRun is how long a holder runs before status, the TUI and the
// narrator call it overdue. Saddle never kills it for that (docs/runq.md Q5).
const DefaultMaxRun = 30 * time.Minute

// MaxRunFor is class's max_run: its own, else the top-level one, else
// DefaultMaxRun.
func (c Config) MaxRunFor(class string) time.Duration {
	if d := c.Classes[class].MaxRun.D; d > 0 {
		return d
	}
	if c.MaxRun.D > 0 {
		return c.MaxRun.D
	}
	return DefaultMaxRun
}

// Duration is a TOML string like "30s".
type Duration struct{ D time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.D = v
	return nil
}

// UserConfigPath is $XDG_CONFIG_HOME/saddle/runq.toml or
// ~/.config/saddle/runq.toml; empty when neither variable is set.
func UserConfigPath(getenv func(string) string) string {
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "saddle", "runq.toml")
}

// LoadConfig reads paths in order, later files overriding earlier ones field
// by field and class by class. Missing files and empty paths are skipped.
func LoadConfig(paths ...string) (Config, error) {
	var c Config
	for _, p := range paths {
		if p == "" {
			continue
		}
		var f Config
		if _, err := toml.DecodeFile(p, &f); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return c, fmt.Errorf("runq config %s: %w", p, err)
		}
		if f.Mode != "" {
			m, err := ParseMode(string(f.Mode))
			if err != nil {
				return c, fmt.Errorf("runq config %s: %w", p, err)
			}
			c.Mode = m
		}
		for _, d := range []struct{ dst, src *Duration }{
			{&c.Heartbeat, &f.Heartbeat}, {&c.StaleAfter, &f.StaleAfter}, {&c.AgingStep, &f.AgingStep},
			{&c.GateMaxWait, &f.GateMaxWait}, {&c.WaitMax, &f.WaitMax}, {&c.MaxRun, &f.MaxRun},
		} {
			if d.src.D > 0 {
				*d.dst = *d.src
			}
		}
		if f.DefaultSlots > 0 {
			c.DefaultSlots = f.DefaultSlots
		}
		if err := f.validate(); err != nil {
			return c, fmt.Errorf("runq config %s: %w", p, err)
		}
		c.merge(f)
		for name, fc := range f.Classes {
			if c.Classes == nil {
				c.Classes = map[string]ClassConfig{}
			}
			cc := c.Classes[name]
			if fc.Slots > 0 {
				cc.Slots = fc.Slots
			}
			if fc.Match != nil {
				cc.Match = fc.Match
			}
			if fc.MaxRun.D > 0 {
				cc.MaxRun = fc.MaxRun
			}
			c.Classes[name] = cc
		}
	}
	return c, nil
}

// validate rejects values that can't mean anything.
func (f Config) validate() error {
	for _, v := range []struct {
		key string
		p   *float64
	}{{"max_load_per_cpu", f.MaxLoadPerCPU}, {"max_cpu_pressure", f.MaxCPUPressure}} {
		if v.p != nil && *v.p < 0 {
			return fmt.Errorf("%s = %v: want >= 0 (0 turns the check off)", v.key, *v.p)
		}
	}
	if f.Nice != nil && (*f.Nice < 0 || *f.Nice > 19) {
		return fmt.Errorf("nice = %d: want 0 to 19", *f.Nice)
	}
	switch f.Scope {
	case "", "none", ScopeSystemd:
	default:
		return fmt.Errorf("scope = %q: want \"systemd\" or \"none\"", f.Scope)
	}
	if f.CPUWeight < 0 || f.CPUWeight > 10000 {
		return fmt.Errorf("cpu_weight = %d: want 1 to 10000", f.CPUWeight)
	}
	if f.TargetUtil < 0 || f.TargetUtil > 1 {
		return fmt.Errorf("target_util = %v: want 0 to 1", f.TargetUtil)
	}
	return nil
}

// merge copies the scheduling keys f sets onto c.
func (c *Config) merge(f Config) {
	if f.MaxLoadPerCPU != nil {
		c.MaxLoadPerCPU = f.MaxLoadPerCPU
	}
	if f.MaxCPUPressure != nil {
		c.MaxCPUPressure = f.MaxCPUPressure
	}
	if f.Nice != nil {
		c.Nice = f.Nice
	}
	if f.IONice != nil {
		c.IONice = f.IONice
	}
	if f.Scope != "" {
		c.Scope = f.Scope
	}
	if f.CPUWeight > 0 {
		c.CPUWeight = f.CPUWeight
	}
	if f.CPUQuota != "" {
		c.CPUQuota = f.CPUQuota
	}
	if f.AdaptiveSlots != nil {
		c.AdaptiveSlots = f.AdaptiveSlots
	}
	if f.TargetUtil > 0 {
		c.TargetUtil = f.TargetUtil
	}
}

// Apply sets the options c configures. Slots start from DefaultClasses.
// Unset keys take the defaults: a LoadGate on /proc (SADDLE_RUNQ_PROC in
// tests) at DefaultMaxLoadPerCPU and DefaultMaxCPUPressure, children at
// nice DefaultNice with idle I/O, no scope, static slots.
func (c Config) Apply(o Options) Options {
	if c.Mode != "" {
		o.Mode = c.Mode
	}
	if c.Heartbeat.D > 0 {
		o.Heartbeat = c.Heartbeat.D
	}
	if c.StaleAfter.D > 0 {
		o.StaleAfter = c.StaleAfter.D
	}
	if c.AgingStep.D > 0 {
		o.AgingStep = c.AgingStep.D
	}
	if c.GateMaxWait.D > 0 {
		o.GateMaxWait = c.GateMaxWait.D
	}
	if c.DefaultSlots > 0 {
		o.DefaultSlots = c.DefaultSlots
	}
	if o.Slots == nil {
		o.Slots = maps.Clone(DefaultClasses)
	}
	g := LoadGate{MaxLoadPerCPU: orDefault(c.MaxLoadPerCPU, DefaultMaxLoadPerCPU),
		MaxPSISomeAvg10: orDefault(c.MaxCPUPressure, DefaultMaxCPUPressure)}
	if g.MaxLoadPerCPU > 0 || g.MaxPSISomeAvg10 > 0 {
		root := ""
		if o.Getenv != nil {
			root = o.Getenv(EnvProcRoot)
		}
		g.Source = ProcLoad{Root: root}
		o.Gate = g
	}
	o.Nice = orDefault(c.Nice, DefaultNice)
	o.IOIdle = orDefault(c.IONice, true)
	if c.Scope == ScopeSystemd {
		o.Scope, o.CPUWeight, o.CPUQuota = ScopeSystemd, c.CPUWeight, c.CPUQuota
		if o.CPUWeight == 0 {
			o.CPUWeight = DefaultCPUWeight
		}
	}
	if orDefault(c.AdaptiveSlots, false) {
		o.TargetUtil = c.TargetUtil
		if o.TargetUtil == 0 {
			o.TargetUtil = DefaultTargetUtil
		}
	}
	for name, cc := range c.Classes {
		if cc.Slots > 0 {
			o.Slots[name] = cc.Slots
		}
	}
	return o
}

func orDefault[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
