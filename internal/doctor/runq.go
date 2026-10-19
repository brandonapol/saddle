package doctor

import (
	"os"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/runq"
)

// runq reports the heavy-run queue's effective mode. Observe, the default,
// records runs but never makes one wait, so the slots, budget and load gate
// do nothing; that is the right start for a measuring week and the wrong
// place to stay (#664).
func (r *run) runq() Result {
	h := app.Heavy{Root: r.env.Root(), Base: &r.cfg.RunQueue}
	cfg, err := h.Config()
	if err != nil {
		return warn(CheckRunq, err.Error(), "fix .saddle/runq.toml (or ~/.config/saddle/runq.toml); the queue falls back to its defaults")
	}
	mode := cfg.Mode
	if m, err := runq.ParseMode(os.Getenv(runq.EnvBypass)); err == nil {
		mode = m
	}
	switch mode {
	case runq.ModeEnforce:
		return ok(CheckRunq, "enforce: heavy runs wait for a slot, the budget and the load gate")
	case runq.ModeOff:
		return ok(CheckRunq, "off: heavy runs are not queued")
	}
	return warn(CheckRunq, "observe: the queue only records heavy runs and never makes one wait, so slots, budget and the load gate do nothing",
		"after a week of measuring, set `mode = \"enforce\"` in .saddle/runq.toml (this repo) or ~/.config/saddle/runq.toml (the machine); `saddle runq status` shows the queue")
}
