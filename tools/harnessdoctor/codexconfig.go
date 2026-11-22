package main

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Project TOML can register a custom role before Codex discovers its managed
// file. Inspect the hook and agent declarations without rewriting local settings.
func checkCodexConfig(root string, roles []agent) error {
	const name = ".codex/config.toml"
	raw, err := regularFile(root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := checkCodexDeclarations(string(raw), roles); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func checkCodexDeclarations(source string, roles []agent) error {
	masked, err := maskTOMLStrings(source)
	if err != nil {
		return err
	}
	statements, err := splitTOML(masked, '\n')
	if err != nil {
		return err
	}
	var table []string
	for _, line := range statements {
		if strings.HasPrefix(line, "[") {
			header := strings.TrimPrefix(line, "[")
			header, ok := strings.CutSuffix(header, "]")
			if !ok {
				return fmt.Errorf("invalid TOML table header %q", line)
			}
			// [[tables]] and [tables] both register their full paths.
			if strings.HasPrefix(header, "[") && strings.HasSuffix(header, "]") {
				header = header[1 : len(header)-1]
			}
			table, err = tomlKeys(header)
			if err == nil {
				err = checkCodexField(table, "", roles)
			}
		} else {
			err = checkCodexAssignment(table, line, roles)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func checkCodexAssignment(parent []string, line string, roles []agent) error {
	field, value, ok := strings.Cut(line, "=")
	if !ok || strings.TrimSpace(value) == "" {
		return fmt.Errorf("invalid TOML assignment %q", field)
	}
	keys, err := tomlKeys(field)
	if err != nil {
		return err
	}
	return checkCodexField(append(parent, keys...), strings.TrimSpace(value), roles)
}

func checkCodexField(path []string, value string, roles []agent) error {
	if len(path) == 1 {
		// Inline and dotted declarations have the same ownership as tables.
		if slices.Contains([]string{"hooks", "features", "agents"}, path[0]) && strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}") {
			fields, err := splitTOML(value[1:len(value)-1], ',')
			if err != nil {
				return err
			}
			for _, field := range fields {
				if err := checkCodexAssignment(path, field, roles); err != nil {
					return err
				}
			}
		}
		return nil
	}
	switch path[0] {
	case "features":
		if len(path) == 2 && (path[1] == "hooks" || path[1] == "codex_hooks") && value == "false" {
			return fmt.Errorf("features.%s disables project hooks; enable the SessionStart and PreToolUse policies", path[1])
		}
	case "hooks":
		if path[1] != "state" {
			return fmt.Errorf("unregistered project hook %s; register the handler in harness/hooks/codex.json before syncing", strings.Join(path, "."))
		}
	case "agents":
		for _, role := range roles {
			if path[1] == role.Name {
				return fmt.Errorf("project declaration %s shadows .codex/agents/%s.toml; remove the duplicate role declaration", strings.Join(path, "."), role.Name)
			}
		}
		if len(path) == 2 && slices.Contains([]string{"model", "model_reasoning_effort", "default_subagent_model", "default_subagent_reasoning_effort"}, path[1]) {
			return fmt.Errorf("agents.%s overrides inheritance; remove the project subagent override", path[1])
		}
	}
	return nil
}

var tomlBareKey = regexp.MustCompile("^[a-zA-Z0-9_-]+$")

func tomlKeys(raw string) ([]string, error) {
	keys := strings.Split(raw, ".")
	for i, key := range keys {
		key = strings.TrimSpace(key)
		// Quoted keys have already been decoded or masked as a single string.
		if len(key) >= 2 && key[0] == '"' && key[len(key)-1] == '"' {
			keys[i] = key[1 : len(key)-1]
		} else if tomlBareKey.MatchString(key) {
			keys[i] = key
		} else {
			return nil, fmt.Errorf("invalid TOML key %q", key)
		}
	}
	return keys, nil
}

// Preserve ASCII key names (including escaped ones); the contents of all other
// quoted strings are opaque to policy. A prompt can contain an entire TOML example.
func maskTOMLStrings(text string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(text); {
		quote := text[i]
		switch quote {
		case '#':
			n := strings.IndexByte(text[i:], '\n')
			if n == -1 {
				return out.String(), nil
			}
			i += n
		case '\'', '"':
			start := i
			width := 1
			if i+2 < len(text) && text[i+1] == quote && text[i+2] == quote {
				width = 3
			}
			i += width
			for {
				if i >= len(text) || width == 1 && text[i] == '\n' {
					return "", errors.New("unterminated TOML string")
				}
				if quote == '"' && text[i] == '\\' {
					i += 2
					continue
				}
				if !strings.HasPrefix(text[i:], text[start:start+width]) {
					i++
					continue
				}
				out.WriteByte('"')
				if width == 1 {
					key := text[start+1 : i]
					if quote == '"' && strings.Contains(key, "\\") {
						key, _ = strconv.Unquote(text[start : i+1])
					}
					if tomlBareKey.MatchString(key) {
						out.WriteString(key)
					}
				}
				out.WriteByte('"')
				i += width
				// Multiline values may end in one or two literal quotes.
				if width == 3 {
					for extra := 0; extra < 2 && i < len(text) && text[i] == quote; extra++ {
						i++
					}
				}
				break
			}
		default:
			out.WriteByte(quote)
			i++
		}
	}
	return out.String(), nil
}

// Split only at the current nesting level, after strings and comments are masked.
func splitTOML(text string, separator byte) ([]string, error) {
	var fields []string
	var closing []byte
	start := 0
	for i := 0; i < len(text); i++ {
		switch c := text[i]; c {
		case '[':
			closing = append(closing, ']')
		case '{':
			closing = append(closing, '}')
		case ']', '}':
			if len(closing) == 0 || closing[len(closing)-1] != c {
				return nil, errors.New("unmatched TOML bracket")
			}
			closing = closing[:len(closing)-1]
		case separator:
			if len(closing) == 0 {
				if field := strings.TrimSpace(text[start:i]); field != "" {
					fields = append(fields, field)
				}
				start = i + 1
			}
		}
	}
	if len(closing) != 0 {
		return nil, errors.New("unterminated TOML table or array")
	}
	if field := strings.TrimSpace(text[start:]); field != "" {
		fields = append(fields, field)
	}
	return fields, nil
}
