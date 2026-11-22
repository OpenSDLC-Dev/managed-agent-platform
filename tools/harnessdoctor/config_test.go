package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCodexProjectConfigOwnsOnlyRegisteredRoles(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	const file = ".codex/config.toml"
	for _, declaration := range []string{
		"[agents.reviewer]\nconfig_file = './custom.toml'\n",
		"[agents.'reviewer'.features]\nshell_tool = true\n",
		"agents . \"revi\\u0065wer\" . developer_instructions = 'replacement'\n",
		"[agents]\nreviewer = { features = { shell_tool = true } }\n",
		"agents = { other = { description = 'a,b' }, reviewer = {} }\n",
		"[agents.reviewer]\n",
	} {
		put(t, root, file, declaration)
		got := call(t, root, 1, "sync")
		if !strings.Contains(got, file) || !strings.Contains(got, ".codex/agents/reviewer.toml") || read(t, root, file) != declaration {
			t.Fatalf("config %q: %s", declaration, got)
		}
	}
	if got := call(t, root, 0, "session-start", "codex"); !strings.Contains(got, "FAIL") || !strings.Contains(got, file) {
		t.Fatal(got)
	}
	unrelated := `
model = 'development'
model_reasoning_effort = 'high'
developer_instructions = """
Examples can include # comments, [brackets], and quoted TOML:
[agents.reviewer]
config_file = "example.toml"
"""
examples = ["[agents.reviewer]", "{ model = 'sample' }"]
[agents]
max_threads = 4
[agents.other]
config_file = "agents/other.toml"
[agents.'reviewer.local']
description = "model = 'example' # just text"
["agents.reviewer"]
notes = "a quoted key containing a literal dot"
[hooks.state."a.b"]
enabled = true
`
	put(t, root, file, unrelated)
	call(t, root, 0, "sync")
	if read(t, root, file) != unrelated {
		t.Fatal("sync changed independently maintained project configuration")
	}
}

func TestClaudeProjectChildSettings(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	const file = ".claude/settings.json"
	var settings map[string]any
	if err := json.Unmarshal([]byte(read(t, root, file)), &settings); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"CLAUDE_CODE_SUBAGENT_EFFORT":      "low",
		"CLAUDE_CODE_SUBAGENT_MODEL_FORCE": "1",
	} {
		settings["env"] = map[string]string{key: value}
		source, _ := formattedJSON(settings)
		put(t, root, file, string(source))
		if got := call(t, root, 1, "sync"); !strings.Contains(got, key) || read(t, root, file) != string(source) {
			t.Fatal(got)
		}
	}
	settings["model"] = "development"
	settings["effortLevel"] = "high"
	settings["maxEffortLevel"] = "high"
	settings["env"] = map[string]string{
		"CLAUDE_CODE_EFFORT_LEVEL":         "high",
		"CLAUDE_CODE_SUBAGENT_MODEL_FORCE": "0",
		"CLAUDE_CODE_SUBAGENT_MODEL":       "inherit",
	}
	source, _ := formattedJSON(settings)
	put(t, root, file, string(source))
	call(t, root, 0, "sync")
	if read(t, root, file) != string(source) {
		t.Fatal("sync changed session-wide settings")
	}
}

func TestCodexDedicatedRegistryRejectsExtraRootFields(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	for _, file := range []string{".codex/hooks.json", "harness/hooks/codex.json"} {
		previous := read(t, root, file)
		withExtra := strings.Replace(previous, "{", `{"disableAllHooks":true,`, 1)
		put(t, root, file, withExtra)
		if got := call(t, root, 1, "sync"); !strings.Contains(got, file) || !strings.Contains(got, "disableAllHooks") || read(t, root, file) != withExtra {
			t.Fatal(got)
		}
		put(t, root, file, previous)
	}
}

func TestCodexProjectConfigRequiresValidTOML(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	const name = ".codex/config.toml"
	for _, text := range []string{
		"[features]\r\nhooks = flase\r\n",
		"allowed_permissions = [\"one\" \"two\"]\r\n",
		`model = "mis\qprint"`,
		"example = 1\r\nexample = 2\r\n",
	} {
		put(t, root, name, text)
		for _, args := range [][]string{{"check", "--host", "codex"}, {"sync"}} {
			if got := call(t, root, 1, args...); !strings.Contains(got, name) || read(t, root, name) != text {
				t.Fatalf("%v: %s", args, got)
			}
		}
		if got := call(t, root, 0, "session-start", "codex"); !strings.Contains(got, "FAIL") || !strings.Contains(got, name) {
			t.Fatal(got)
		}
	}
}
