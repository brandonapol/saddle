package mcpserver

import (
	"errors"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
)

type ConfigIn struct {
	Key   string `json:"key" jsonschema:"dotted setting, e.g. adapters.grok.disabled, adapters.order, train.lint.cmd"`
	Value string `json:"value,omitempty" jsonschema:"TOML value to set, e.g. true, 5, [\"claude\", \"codex\"], or a bare string; omit to read the key"`
}

type ConfigOut struct {
	Key   string `json:"key"`
	Value string `json:"value" jsonschema:"the value in .saddle/config.toml as TOML; empty when unset (saddle's default applies)"`
	Line  string `json:"line,omitempty" jsonschema:"the line written, on a set"`
}

const configDesc = "Read or change one setting in this repo's .saddle/config.toml without editing the file: key alone reads it; key and value set it, keeping the file's comments. " +
	"A key saddle doesn't read, or a value it rejects, leaves the file unchanged. Adapter settings and the train's take effect on the next spawn or land, without a restart. " +
	"E.g. key adapters.grok.disabled, value true keeps grok out of every spawn. Orchestrator only, and only change a setting when the owner asks."

func configTool(a *app.App, task string, in ConfigIn) (ConfigOut, error) {
	out := ConfigOut{Key: in.Key}
	if strings.TrimSpace(in.Key) == "" {
		return out, errors.New("key is required, e.g. adapters.grok.disabled")
	}
	if in.Value == "" {
		v, err := a.GetConfig(in.Key)
		out.Value = v
		return out, err
	}
	if task != app.OrchestratorID {
		return out, fmt.Errorf("only the orchestrator can change saddle's config; ask it to set %s", in.Key)
	}
	line, err := a.SetConfig(in.Key, in.Value)
	if err != nil {
		return out, err
	}
	out.Line = line
	out.Value, err = a.GetConfig(in.Key)
	return out, err
}
