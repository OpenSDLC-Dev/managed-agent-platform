package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
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
	if host == "claude" {
		env, _ := have["env"].(map[string]any)
		for _, key := range []string{"CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL", "CLAUDE_CODE_SUBAGENT_EFFORT"} {
			value, present := env[key]
			if present && value != "" && !(key == "CLAUDE_CODE_SUBAGENT_MODEL" && value == "inherit") {
				return nil, fmt.Errorf("env.%s overrides inheritance; remove the project override", key)
			}
		}
		// FORCE can activate a subagent model supplied by another configuration scope.
		switch strings.ToLower(fmt.Sprint(env["CLAUDE_CODE_SUBAGENT_MODEL_FORCE"])) {
		case "1", "true", "yes", "on":
			return nil, errors.New("env.CLAUDE_CODE_SUBAGENT_MODEL_FORCE forces the child model; remove the project override")
		}
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
		a := []any{}
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
		if !ok || len(a) > len(b) {
			return false
		}
		// A partial handler can match either slot when one carries extra
		// settings. Let an existing assignment move to another matching slot.
		owners := make(map[int]int)
		var assign func(int, []bool) bool
		assign = func(index int, visited []bool) bool {
			for slot, candidate := range b {
				if visited[slot] || !registeredShape(a[index], candidate, field) {
					continue
				}
				visited[slot] = true
				if previous, taken := owners[slot]; !taken || assign(previous, visited) {
					owners[slot] = index
					return true
				}
			}
			return false
		}
		for index := range a {
			if !assign(index, make([]bool, len(b))) {
				return false
			}
		}
		return true
	default:
		switch field {
		case "command", "commandWindows", "command_windows", "type":
			return reflect.DeepEqual(actual, expected)
		}
		return reflect.TypeOf(actual) == reflect.TypeOf(expected)
	}
}

// Native discovery must reach the worktree checks even when a source and
// its generated adapter drift together. Git shell aliases execute at the root.
func callsGo(command, alias, target string) bool {
	return command == `git -c "alias.`+alias+"=!"+target+`" `+alias
}

// Codex deserializes the entire registry before it discovers any handler.
// Apply its command field types to auxiliary hooks as well as the required two.
func codexCommandFields(event string, h map[string]any) error {
	if _, camel := h["commandWindows"]; camel {
		if _, snake := h["command_windows"]; snake {
			return fmt.Errorf("%s: commandWindows and command_windows declare the same Codex field twice", event)
		}
	}
	for _, key := range []string{"commandWindows", "command_windows", "statusMessage"} {
		if value := h[key]; value != nil {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s: %s must be a string or null", event, key)
			}
		}
	}
	if value, present := h["async"]; present {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: async must be a boolean", event)
		}
	}
	for _, key := range []string{"timeout", "additionalContextLimit"} {
		value := h[key]
		if value == nil {
			continue
		}
		bits := 64 // timeout is a Rust u64; the context limit is a usize.
		if key == "additionalContextLimit" {
			bits = strconv.IntSize
		}
		number, _ := value.(json.Number)
		n, err := strconv.ParseUint(number.String(), 10, bits)
		if err != nil {
			return fmt.Errorf("%s: %s must be an unsigned %d-bit integer or null", event, key, bits)
		}
		if n > math.MaxInt64 {
			// Codex hashes the normalized handler as TOML, whose integers are
			// signed. SessionEnd timeouts are clamped before hashing; only the
			// events below keep their additional-context limit in the identity.
			contextEvents := []string{"PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit", "SubagentStart"}
			if key == "timeout" && event != "SessionEnd" || key == "additionalContextLimit" && slices.Contains(contextEvents, event) {
				return fmt.Errorf("%s: %s must fit in a signed 64-bit TOML integer for Codex to register the hook", event, key)
			}
		}
	}
	return nil
}

func requiredHookOptions(host, event string, h map[string]any) error {
	for _, field := range []string{"async", "asyncRewake"} {
		if value, present := h[field]; present {
			if field == "asyncRewake" && host == "codex" {
				return fmt.Errorf("%s: Codex does not support %s; remove the field", event, field)
			}
			if value != false {
				return fmt.Errorf("%s must run synchronously; omit %s or set it to false", event, field)
			}
		}
	}
	if host == "codex" {
		for _, key := range []string{"commandWindows", "command_windows"} {
			if value := h[key]; value != nil && value != h["command"] {
				return fmt.Errorf("%s: %s must run the registered command on Windows", event, key)
			}
		}
	}
	return nil
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
	if host == "codex" || (host == "claude" && canonical) {
		for key := range doc {
			if key != "hooks" {
				return fmt.Errorf("unregistered top-level key %q in %s hook registry; inspect it before sync", key, host)
			}
		}
	}
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
			if host == "codex" {
				if err := codexCommandFields(event, h); err != nil {
					return err
				}
			}
			startup := event == "SessionStart" && callsGo(command, "map-doctor", "go run ./tools/harnessdoctor session-start "+host)
			preTool := event == "PreToolUse" && callsGo(command, "map-pretool", "go run ./tools/harnesshook "+host)
			if canonical && (startup || preTool) {
				if err := requiredHookOptions(host, event, h); err != nil {
					return err
				}
			}
			if startup {
				starts++
			}
			if preTool && matchesPreTool(host, matcher) {
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
						if err != nil || !expr.MatchString("startup") || !expr.MatchString("fork") || !expr.MatchString("clear") {
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
