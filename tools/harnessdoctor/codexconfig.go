package main

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/tools/harnessdoctor/internal/toml"
)

// Codex reads project TOML before discovering the managed role files. Parse the
// complete document, then inspect only the settings which affect this harness.
func checkCodexConfig(root string, roles []agent) error {
	const name = ".codex/config.toml"
	raw, err := regularFile(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var config map[string]any
	if _, err := toml.Decode(string(raw), &config); err != nil {
		var syntax toml.ParseError
		if errors.As(err, &syntax) {
			return fmt.Errorf("%s: invalid TOML at line %d, column %d; correct the syntax and rerun check", name, syntax.Position.Line, syntax.Position.Col)
		}
		return fmt.Errorf("%s: cannot decode the TOML document; correct it and rerun check", name)
	}
	for _, section := range []string{"features", "hooks", "agents"} {
		value, exists := config[section]
		if !exists {
			continue
		}
		table, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %s must be a TOML table", name, section)
		}
		if err := checkProjectTable(section, table, roles); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func checkProjectTable(section string, table map[string]any, roles []agent) error {
	for _, key := range slices.Sorted(maps.Keys(table)) {
		switch section {
		case "features":
			if key == "codex_hooks" || key == "hooks" {
				if enabled, ok := table[key].(bool); !ok || !enabled {
					return fmt.Errorf("features.%s must be true to run SessionStart and PreToolUse", key)
				}
			}
		case "hooks":
			if key != "state" {
				return fmt.Errorf("unregistered project hook hooks.%s; register the handler in harness/hooks/codex.json before syncing", key)
			}
		case "agents":
			for _, role := range roles {
				if key == role.Name {
					return fmt.Errorf("project declaration agents.%s shadows .codex/agents/%s.toml; remove the duplicate role declaration", key, key)
				}
			}
			if slices.Contains([]string{"model", "model_reasoning_effort", "default_subagent_model", "default_subagent_reasoning_effort"}, key) {
				return fmt.Errorf("agents.%s overrides inheritance; remove the project subagent override", key)
			}
		}
	}
	return nil
}
