package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// settingsProblems reports syntax and misplaced Saddle rules before treating
// the merged allowlist as usable. Claude ignores an invalid settings file.
func settingsProblems(root string) (invalid, warnings []string) {
	for _, name := range []string{"settings.json", "settings.local.json"} {
		path := filepath.Join(root, ".claude", name)
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		var st struct {
			Permissions struct {
				Allow []string `json:"allow"`
			} `json:"permissions"`
			Servers []string `json:"enabledMcpjsonServers"`
		}
		if err == nil {
			err = json.Unmarshal(b, &st)
		}
		if err != nil || strings.TrimSpace(string(b)) == "null" {
			invalid = append(invalid, fmt.Sprintf("%s: invalid Claude settings (%v); every rule in this file is ignored", path, err))
			continue
		}
		for _, rule := range st.Permissions.Allow {
			if strings.HasPrefix(rule, "mcp_saddle") {
				warnings = append(warnings, fmt.Sprintf("%s: permissions.allow entry %q uses single underscores; use mcp__saddle or mcp__saddle__<tool>", path, rule))
			}
		}
		for _, server := range st.Servers {
			if strings.HasPrefix(server, "mcp__saddle") || strings.HasPrefix(server, "mcp_saddle") {
				warnings = append(warnings, fmt.Sprintf("%s: enabledMcpjsonServers entry %q is a tool name; the server name is saddle", path, server))
			}
		}
	}
	return invalid, warnings
}

// fixOrchAllow atomically merges the two rules, preserving unknown settings
// and permission fields. Never replace a file whose syntax or shape is invalid.
func fixOrchAllow(root string) error {
	dir := filepath.Join(root, ".claude")
	path := filepath.Join(dir, "settings.local.json")
	st := map[string]json.RawMessage{}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(b, &st); err != nil || st == nil {
			return fmt.Errorf("%s: repair invalid JSON before saddle doctor --fix", path)
		}
	}
	permissions := map[string]json.RawMessage{}
	if raw, ok := st["permissions"]; ok {
		if err := json.Unmarshal(raw, &permissions); err != nil || permissions == nil {
			return fmt.Errorf("%s: permissions must be an object", path)
		}
	}
	var allow []string
	if raw, ok := permissions["allow"]; ok {
		if err := json.Unmarshal(raw, &allow); err != nil {
			return fmt.Errorf("%s: permissions.allow must be a list of strings", path)
		}
	}
	for _, rule := range orchAllowRules {
		if !slices.Contains(allow, rule) {
			allow = append(allow, rule)
		}
	}
	permissions["allow"], err = json.Marshal(allow)
	if err != nil {
		return err
	}
	st["permissions"], err = json.Marshal(permissions)
	if err != nil {
		return err
	}
	b, err = json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".saddle-settings-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
