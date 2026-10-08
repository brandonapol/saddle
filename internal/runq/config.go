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
//	default_slots = 1        # for classes named nowhere
//	[classes.go-test]
//	slots = 2
//	match = ["go test*", "make check"]
type Config struct {
	Mode         Mode                   `toml:"mode"`
	Heartbeat    Duration               `toml:"heartbeat"`
	StaleAfter   Duration               `toml:"stale_after"`
	AgingStep    Duration               `toml:"aging_step"`
	GateMaxWait  Duration               `toml:"gate_max_wait"`
	WaitMax      Duration               `toml:"wait_max"`
	DefaultSlots int                    `toml:"default_slots"`
	Classes      map[string]ClassConfig `toml:"classes"`
}

// ClassConfig is one [classes.<name>] table. Match holds the argv patterns
// the hook and shims (#240) route to the class; the core doesn't read them.
type ClassConfig struct {
	Slots int      `toml:"slots"`
	Match []string `toml:"match"`
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
			{&c.GateMaxWait, &f.GateMaxWait}, {&c.WaitMax, &f.WaitMax},
		} {
			if d.src.D > 0 {
				*d.dst = *d.src
			}
		}
		if f.DefaultSlots > 0 {
			c.DefaultSlots = f.DefaultSlots
		}
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
			c.Classes[name] = cc
		}
	}
	return c, nil
}

// Apply sets the options c configures. Slots start from DefaultClasses.
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
	for name, cc := range c.Classes {
		if cc.Slots > 0 {
			o.Slots[name] = cc.Slots
		}
	}
	return o
}
