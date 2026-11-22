package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

var hosts = []string{"claude", "codex", "antigravity"}

var registryPaths = map[string]string{
	"claude":      ".claude/settings.json",
	"codex":       ".codex/hooks.json",
	"antigravity": ".agents/hooks.json",
}

// mergeRegistry repairs missing registrations. An unknown command, unknown field
// or malformed document may belong to a maintainer and is never overwritten.
func mergeRegistry(host string, source, existing []byte) ([]byte, error) {
	wanted, err := jsonObject(source)
	if err != nil {
		return nil, fmt.Errorf("invalid canonical hook JSON: %w", err)
	}
	if err := validateRegistry(host, wanted, true); err != nil {
		return nil, fmt.Errorf("canonical hooks: %w", err)
	}
	if existing == nil {
		return formattedJSON(wanted)
	}
	have, err := jsonObject(existing)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON; repair the original before sync: %w", err)
	}
	if err = validateRegistry(host, have, false); err != nil {
		return nil, err
	}
	if host == "claude" && have["disableAllHooks"] == true {
		return nil, errors.New("disableAllHooks prevents SessionStart; review this project setting")
	}
	var a, b any = have, wanted
	if host == "claude" {
		if _, ok := have["hooks"]; !ok {
			have["hooks"] = map[string]any{}
		}
		a, b = have["hooks"], wanted["hooks"]
	}
	if reflect.DeepEqual(a, b) {
		return existing, nil
	}
	if !registeredShape(a, b, "") {
		return nil, errors.New("unregistered hook or field; register it in the canonical policy or move it to local settings before sync")
	}
	if host == "claude" {
		have["hooks"] = b
		return formattedJSON(have)
	}
	return formattedJSON(wanted)
}

func formattedJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	err := enc.Encode(v)
	return buf.Bytes(), err
}

// Decode objects without discarding duplicate keys or rounding an unrelated numeric setting.
func jsonObject(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("JSON must be valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeJSON(dec)
	if err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON content")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("expected JSON object")
	}
	return obj, nil
}

func decodeJSON(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, errors.New("invalid JSON object key")
			}
			if _, ok = m[key]; ok {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			m[key], err = decodeJSON(dec)
			if err != nil {
				return nil, err
			}
		}
		_, err = dec.Token()
		return m, err
	case json.Delim('['):
		var a []any
		for dec.More() {
			v, err := decodeJSON(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = dec.Token()
		return a, err
	default:
		return t, nil
	}
}

// Compare handlers by identity, not array position. Missing registrations can be
// restored even when the existing groups are reordered.
func registeredShape(actual, expected any, field string) bool {
	switch a := actual.(type) {
	case map[string]any:
		b, ok := expected.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range a {
			w, ok := b[k]
			if !ok || !registeredShape(v, w, k) {
				return false
			}
		}
		return true
	case []any:
		b, ok := expected.([]any)
		if !ok {
			return false
		}
		used := make([]bool, len(b))
		for _, v := range a {
			found := false
			for i, w := range b {
				if !used[i] && registeredShape(v, w, field) {
					used[i] = true
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	default:
		if field == "command" || field == "type" {
			return reflect.DeepEqual(actual, expected)
		}
		return reflect.TypeOf(actual) == reflect.TypeOf(expected)
	}
}

// Native discovery must reach the worktree checks even when a source and
// its generated adapter drift together. Git shell aliases execute at the root.
func callsGo(command, target string) bool {
	alias, invocation, ok := strings.Cut(strings.TrimPrefix(command, `git -c "alias.`), `=!`)
	return ok && alias != "" && invocation == target+`" `+alias && strings.HasPrefix(command, `git -c "alias.`)
}

func matchesPreTool(host, matcher string) bool {
	if slices.Contains([]string{"", "*", ".*", "^.*$"}, matcher) {
		return true
	}
	if host == "codex" {
		// The role guard must see every tool, including tools added by a plugin.
		return false
	}
	if host == "antigravity" {
		return matcher == "run_command"
	}
	expr, err := regexp.Compile("^(?:" + matcher + ")$")
	return err == nil && expr.MatchString("Bash") && expr.MatchString("PowerShell")
}

func validateRegistry(host string, doc map[string]any, canonical bool) error {
	starts, gates := 0, 0
	check := func(event, matcher string, handlers any) error {
		list, ok := handlers.([]any)
		if !ok || len(list) == 0 {
			return fmt.Errorf("invalid %s handler list", event)
		}
		for _, item := range list {
			h, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid %s handler", event)
			}
			command, _ := h["command"].(string)
			if h["type"] != "command" || command == "" {
				return fmt.Errorf("unregistered or malformed %s command; inspect it before sync", event)
			}
			if event == "SessionStart" && callsGo(command, "go run ./tools/harnessdoctor session-start "+host) {
				starts++
			}
			if event == "PreToolUse" && callsGo(command, "go run ./tools/harnesshook "+host) && matchesPreTool(host, matcher) {
				gates++
			}
		}
		return nil
	}
	events := func(group map[string]any) error {
		for event, value := range group {
			if host == "antigravity" && event == "enabled" {
				enabled, ok := value.(bool)
				if !ok || canonical && !enabled {
					return errors.New("invalid or disabled canonical hook")
				}
				continue
			}
			if host == "antigravity" && event == "SessionStart" {
				if err := check(event, "", value); err != nil {
					return err
				}
				continue
			}
			groups, ok := value.([]any)
			if !ok {
				return fmt.Errorf("%s must contain hook groups", event)
			}
			for _, item := range groups {
				g, ok := item.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid %s hook group", event)
				}
				if matcher, exists := g["matcher"]; exists {
					pattern, ok := matcher.(string)
					if !ok {
						return fmt.Errorf("invalid %s matcher", event)
					}
					if canonical && event == "SessionStart" && pattern != "*" && pattern != "" {
						expr, err := regexp.Compile(pattern)
						if err != nil || !expr.MatchString("startup") {
							return fmt.Errorf("%s matcher %q excludes new sessions", event, pattern)
						}
					}
				}
				matcher, _ := g["matcher"].(string)
				if err := check(event, matcher, g["hooks"]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if host == "antigravity" {
		for name, value := range doc {
			group, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("invalid hook group %s", name)
			}
			if err := events(group); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	} else if raw, exists := doc["hooks"]; exists {
		group, ok := raw.(map[string]any)
		if !ok {
			return errors.New("hooks must be an object")
		}
		if err := events(group); err != nil {
			return err
		}
	}
	if canonical && starts != 1 {
		return fmt.Errorf("expected one SessionStart command for %s; found %d", host, starts)
	}
	if canonical && gates != 1 {
		return fmt.Errorf("expected one PreToolUse command covering %s policy tools; found %d", host, gates)
	}
	return nil
}
