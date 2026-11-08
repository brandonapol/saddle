package app

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/brandonapol/saddle/internal/config"
)

// EventConfigSet records a setting changed with saddle config set.
const EventConfigSet = "config_set"

func (a *App) configPath() string { return filepath.Join(a.Root, ".saddle", "config.toml") }

// SetConfig sets the dotted key to value in .saddle/config.toml, keeping
// the file's comments, and applies it to this saddle (#908). It refuses a
// key saddle doesn't read and a value Load rejects, leaving the file as it
// was. It returns the line written.
func (a *App) SetConfig(key, value string) (string, error) {
	p := a.configPath()
	old, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	out, err := config.SetKey(string(old), key, value)
	if err != nil {
		return "", err
	}
	if config.UnknownKey(out, key) {
		return "", fmt.Errorf("config key %q: saddle has no such setting; see the commented template in .saddle/config.toml (saddle init writes it)", key)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(out), 0o644); err != nil {
		return "", err
	}
	if _, err := config.Load(a.Root); err != nil {
		restore := os.WriteFile(p, old, 0o644)
		if old == nil {
			restore = os.Remove(p)
		}
		return "", errors.Join(fmt.Errorf("%s = %s: %w; .saddle/config.toml is unchanged", key, value, err), restore)
	}
	line := setLine(out, key)
	a.Store.Event("", EventConfigSet, key+": "+line)
	if _, err := a.ReloadTrainConfig(); err != nil {
		return line, err
	}
	return line, nil
}

// setLine is the line of out that SetKey wrote for key.
func setLine(out, key string) string {
	parts := strings.Split(key, ".")
	for i := range parts {
		prefix := strings.Join(parts[i:], ".") + " ="
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), prefix) {
				return strings.TrimSpace(l)
			}
		}
	}
	return key
}

// GetConfig is the dotted key's value in .saddle/config.toml, as TOML, or
// "" when the file doesn't set it (saddle's default applies).
func (a *App) GetConfig(key string) (string, error) {
	var m map[string]any
	if _, err := toml.DecodeFile(a.configPath(), &m); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	var v any = m
	for _, p := range strings.Split(key, ".") {
		t, ok := v.(map[string]any)
		if !ok {
			return "", nil
		}
		if v, ok = t[p]; !ok {
			return "", nil
		}
	}
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(map[string]any{"v": v}); err != nil {
		return "", err
	}
	s := strings.TrimSpace(b.String())
	if after, ok := strings.CutPrefix(s, "v = "); ok {
		return after, nil
	}
	return s, nil // a table: its TOML
}
