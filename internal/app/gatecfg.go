package app

import "fmt"

// reloadGateConfig re-reads the gate's config sections before a done runs
// the gate, so a lint.cmd or test.cmd edited in .saddle/config.toml takes
// effect on the next done without restarting saddle (#339), as it does on
// the next land (#228).
func (a *App) reloadGateConfig() error {
	if _, err := a.ReloadTrainConfig(); err != nil {
		return fmt.Errorf("reading .saddle/config.toml for the gate: %w; fix the file and call the saddle done tool again", err)
	}
	return nil
}
