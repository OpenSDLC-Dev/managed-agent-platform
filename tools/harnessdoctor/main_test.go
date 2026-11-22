package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func put(t *testing.T, root, name, value string) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(value), 0644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gitTest(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = gitEnv()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v %s", args, err, output)
	}
}

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitTest(t, root, "init", "-q")
	for path, data := range map[string]string{
		"AGENTS.md":                               "# Project\nRun `go run ./tools/harnessdoctor check`. Review, sync, and check again.",
		"harness/manifest.json":                   `{"skills":["example"],"rules":[{"name":"storage","paths":["src/**/io*.go"]},{"name":"source","paths":["src/**"]}],"agents":[{"name":"reviewer","description":"Review the diff.","claudeTools":["Read"],"antigravityTools":["read_file"],"codexEvidenceOnly":true}]}`,
		"harness/rules/source.md":                 "Source rule.\n",
		"harness/rules/storage.md":                "Storage rule.\n",
		"harness/agents/reviewer.md":              "Read the supplied evidence.\n",
		"harness/skills/example/SKILL.md":         "---\nname: example\ndescription: Use this workflow for its evidence.\n---\n\nRead the reference.\n",
		"harness/skills/example/assets/input.bin": "\x00\x91\n\x02",
		"tools/harnesshook/command.go":            "package main\n",
		"tools/harnesshook/main.go":               "package main\n",
		"harness/hooks/verify-commit-gate.sh":     "#!/bin/sh\n",
		"harness/hooks/verify-stamp.sh":           "#!/bin/sh\n",
		".claude/settings.json":                   `{"permissions":{"deny":["Read(./.env)"]},"custom":{"n":1}}`,
	} {
		put(t, root, path, data)
	}
	for _, client := range []string{"claude", "codex", "antigravity"} {
		data := read(t, filepath.Join("..", ".."), "harness/hooks/"+client+".json")
		put(t, root, "harness/hooks/"+client+".json", data)
	}
	return root
}

func call(t *testing.T, dir string, want int, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	status := run(dir, args, &out)
	if status != want {
		t.Fatalf("doctor %v: code %d want %d: %s", args, status, want, out.String())
	}
	return out.String()
}

func TestDoctorWorkflow(t *testing.T) {
	root := fixture(t)
	call(t, root, 1, "check")
	call(t, root, 0, "sync")
	call(t, root, 0)
	if got := read(t, root, ".claude/settings.json"); !strings.Contains(got, "Read(./.env)") {
		t.Fatal(got)
	}
	if got := read(t, root, ".agents/skills/example/assets/input.bin"); got != read(t, root, "harness/skills/example/assets/input.bin") {
		t.Fatal("binary attachment changed")
	}
	settings := read(t, root, ".claude/settings.json")
	call(t, root, 0, "sync")
	if settings != read(t, root, ".claude/settings.json") {
		t.Fatal("second sync changed settings")
	}
	put(t, root, ".codex/agents/reviewer.toml", "model = \"weaker\"")
	call(t, root, 0, "check", "--host", "claude")
	if got := call(t, root, 1, "check"); !strings.Contains(got, ".codex/agents/reviewer.toml") || !strings.Contains(got, "sync") {
		t.Fatal(got)
	}
	nested := filepath.Join(root, "harness", "rules")
	rules := call(t, nested, 0, "rules", `..\..\src\io_test.go`, "../../src/nested/io.go")
	if strings.TrimSpace(rules) != "harness/rules/source.md\nSource rule.\nharness/rules/storage.md\nStorage rule." {
		t.Fatal(rules)
	}
	if got := call(t, nested, 1, "rules", "../../../outside"); !strings.Contains(got, "outside") {
		t.Fatal(got)
	}
	call(t, root, 0, "sync")
	code := read(t, root, ".codex/agents/reviewer.toml")
	if !strings.Contains(code, "shell_tool = false") || strings.Contains(code, "model =") {
		t.Fatal(code)
	}
}

func TestHookConflictsAndReceipts(t *testing.T) {
	for _, host := range []string{"claude", "codex", "antigravity"} {
		t.Run(host, func(t *testing.T) {
			root := fixture(t)
			call(t, root, 0, "sync")
			file := map[string]string{"claude": ".claude/settings.json", "codex": ".codex/hooks.json", "antigravity": ".agents/hooks.json"}[host]
			original := read(t, root, file)
			for _, bad := range []string{"{", strings.Replace(original, "session-start "+host, "other-command", 1)} {
				put(t, root, file, bad)
				receipt := call(t, root, 0, "session-start", host)
				if !json.Valid([]byte(receipt)) || !strings.Contains(receipt, "harness-doctor: FAIL") || !strings.Contains(receipt, file) {
					t.Fatal(receipt)
				}
				if host == "antigravity" && !strings.Contains(receipt, "injectSteps") {
					t.Fatal(receipt)
				}
				if host != "antigravity" && !strings.Contains(receipt, "additionalContext") {
					t.Fatal(receipt)
				}
				call(t, root, 1, "sync")
				if read(t, root, file) != bad {
					t.Fatal("sync replaced an unknown/malformed hook")
				}
			}
			put(t, root, file, original)
			if r := call(t, root, 0, "session-start", host); !strings.Contains(r, "harness-doctor: OK") {
				t.Fatal(r)
			}
		})
	}
}

func TestSharedSourceValidation(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	put(t, root, "AGENTS.md", strings.Repeat("x\n", 201))
	if got := call(t, root, 1, "check"); !strings.Contains(got, "200") {
		t.Fatal(got)
	}
	put(t, root, "AGENTS.md", "go run ./tools/harnessdoctor check; sync")
	put(t, root, ".agents/skills/extra/SKILL.md", "Unexpected\n")
	if got := call(t, root, 1, "sync"); !strings.Contains(got, ".agents/skills/extra") {
		t.Fatal(got)
	}
	if err := os.Remove(filepath.Join(root, ".agents", "skills", "extra", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	put(t, root, "harness/manifest.json", strings.Replace(read(t, root, "harness/manifest.json"), `"name":"reviewer"`, `"model":"weak","name":"reviewer"`, 1))
	if got := call(t, root, 1, "check"); !strings.Contains(got, "model") {
		t.Fatal(got)
	}
}

func TestRestoreSharedSettingsAndAttachment(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	original := read(t, root, ".claude/settings.json")
	gitTest(t, root, "add", ".claude/settings.json")
	for _, name := range []string{".claude/settings.json", ".agents/skills/example/assets/input.bin"} {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	problems := call(t, root, 1, "check")
	for _, name := range []string{".claude/settings.json", ".agents/skills/example/assets/input.bin"} {
		if !strings.Contains(problems, name) {
			t.Fatal(problems)
		}
	}
	call(t, root, 0, "sync")
	if restored := read(t, root, ".claude/settings.json"); restored != original {
		t.Fatalf("lost staged host settings:\n%s", restored)
	}
	if copied := read(t, root, ".agents/skills/example/assets/input.bin"); copied != read(t, root, "harness/skills/example/assets/input.bin") {
		t.Fatal("attachment not restored")
	}
}

func TestMiswiredSessionHookAndMissingSourceReference(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	name := "harness/hooks/codex.json"
	baseline := read(t, root, name)
	put(t, root, name, strings.Replace(baseline, `"startup|fork"`, `"resume"`, 1))
	if result := call(t, root, 1, "check"); !strings.Contains(result, "excludes new sessions") {
		t.Fatal(result)
	}
	put(t, root, name, baseline)
	put(t, root, "harness/skills/example/SKILL.md", read(t, root, "harness/skills/example/SKILL.md")+"\nRead [schema](./assets/schema.json).\n")
	if result := call(t, root, 1, "check"); !strings.Contains(result, "assets/schema.json") {
		t.Fatal(result)
	}
}

func TestSyncPreflightsAllHookConflicts(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	put(t, root, ".agents/agents/reviewer.md", "hand edit")
	file := ".codex/hooks.json"
	put(t, root, file, `{"hooks":{"SessionStart":[],"SessionStart":[]}}`)
	if problems := call(t, root, 1, "sync"); !strings.Contains(problems, "duplicate JSON key") {
		t.Fatal(problems)
	}
	if content := read(t, root, ".agents/agents/reviewer.md"); content != "hand edit" {
		t.Fatal("sync wrote before checking hook conflicts")
	}
}

func TestLinkedWorktreeAndExecutableAttachment(t *testing.T) {
	base := fixture(t)
	put(t, base, "harness/skills/example/probe.sh", "#!/bin/sh\nexit 0\n")
	script := "harness/skills/example/probe.sh"
	if err := os.Chmod(filepath.Join(base, script), 0755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, base, "add", ".")
	gitTest(t, base, "update-index", "--chmod=+x", script)
	gitTest(t, base, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")
	parent := t.TempDir()
	linked := filepath.Join(parent, "linked tree")
	gitTest(t, base, "worktree", "add", "-q", "--detach", linked)
	nested := filepath.Join(linked, "harness", "skills")
	if runtime.GOOS == "windows" {
		// Git's index records executable bits on a filesystem without POSIX mode bits.
		// Stage only these generated attachments after sync, as the diagnostic requests.
		var buf bytes.Buffer
		run(nested, []string{"sync"}, &buf)
		gitTest(t, linked, "add", "--chmod=+x", ".agents/skills/example/probe.sh", ".claude/skills/example/probe.sh")
	} else {
		call(t, nested, 0, "sync")
	}
	call(t, nested, 0, "check")
	if _, err := os.Stat(filepath.Join(base, ".agents")); !os.IsNotExist(err) {
		t.Fatal("doctor wrote into the main checkout", err)
	}
}

func TestRequiredPreToolUsePolicy(t *testing.T) {
	for _, host := range hosts {
		t.Run(host, func(t *testing.T) {
			root := fixture(t)
			call(t, root, 0, "sync")
			source := "harness/hooks/" + host + ".json"
			original := read(t, root, source)
			wrongEvent := strings.Replace(original, "PreToolUse", "PostToolUse", 1)
			var wrongTool string
			switch host {
			case "claude":
				wrongTool = strings.Replace(original, "Bash|PowerShell", "Read", 1)
			case "antigravity":
				wrongTool = strings.Replace(original, "run_command", "read_file", 1)
			case "codex":
				prefix, group, _ := strings.Cut(original, `"PreToolUse"`)
				wrongTool = prefix + `"PreToolUse"` + strings.Replace(group, `"hooks":`, `"matcher":"Read", "hooks":`, 1)
			}
			for _, broken := range []string{wrongEvent, wrongTool} {
				put(t, root, source, broken)
				put(t, root, registryPaths[host], broken)
				if got := call(t, root, 1, "check"); !strings.Contains(got, source) || !strings.Contains(got, "PreToolUse") {
					t.Fatal(got)
				}
			}
		})
	}
}

func TestRuntimeHookDependencies(t *testing.T) {
	for _, name := range []string{"tools/harnesshook/main.go", "tools/harnesshook/command.go", "harness/hooks/verify-commit-gate.sh", "harness/hooks/verify-stamp.sh"} {
		root := fixture(t)
		call(t, root, 0, "sync")
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
		if got := filepath.ToSlash(call(t, root, 1, "check")); !strings.Contains(got, name) {
			t.Fatal(got)
		}
	}
}

func TestProjectRoleDefaults(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	const config = ".codex/config.toml"
	for _, setting := range []string{
		"[agents]\ndefault_subagent_model = 'weak'",
		"agents = { default_subagent_reasoning_effort = 'low' }",
		"agents.reviewer.model_reasoning_effort = 'low'",
	} {
		put(t, root, config, setting)
		if got := call(t, root, 1, "check", "--host", "codex"); !strings.Contains(got, config) {
			t.Fatal(got)
		}
		call(t, root, 0, "check", "--host", "claude")
	}
	put(t, root, config, "model = 'developer'\n[agents]\nmax_threads = 4\n# model = 'comment'\n")
	call(t, root, 0, "check")
	name := ".claude/settings.json"
	settings := strings.Replace(read(t, root, name), "{", `{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"pinned"},`, 1)
	put(t, root, name, settings)
	if result := call(t, root, 1, "sync"); !strings.Contains(result, "CLAUDE_CODE_SUBAGENT_MODEL") || read(t, root, name) != settings {
		t.Fatal(result)
	}
}

func TestSynchronousClaudeHandlers(t *testing.T) {
	root := fixture(t)
	name := "harness/hooks/claude.json"
	source := read(t, root, name)
	for _, event := range []string{"SessionStart", "PreToolUse"} {
		before, after, _ := strings.Cut(source, event)
		for _, field := range []string{"async", "asyncRewake"} {
			put(t, root, name, before+event+strings.Replace(after, `"type": "command"`, `"`+field+`": true, "type": "command"`, 1))
			if got := call(t, root, 1, "check"); !strings.Contains(got, "synchronously") {
				t.Fatal(got)
			}
		}
	}
}

func TestRecoverSettingsAfterStagedDeletion(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	const name = ".claude/settings.json"
	original := read(t, root, name)
	gitTest(t, root, "add", name)
	gitTest(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "shared settings")
	gitTest(t, root, "rm", "--", name)
	call(t, root, 0, "sync")
	if read(t, root, name) != original {
		t.Fatal("lost independently maintained Claude settings from HEAD")
	}
}

func TestSyncPreservesEmptySettingsArrays(t *testing.T) {
	root := fixture(t)
	const file = ".claude/settings.json"
	put(t, root, file, `{"permissions":{"allow":[],"deny":["Read(./.env)"]},"other":{"list":[]}}`)
	call(t, root, 0, "sync")
	var settings struct {
		Permissions struct{ Allow, Deny []string }
		Other       struct{ List []string }
	}
	if err := json.Unmarshal([]byte(read(t, root, file)), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Permissions.Allow == nil || settings.Other.List == nil || len(settings.Permissions.Deny) != 1 {
		t.Fatal("sync changed independent arrays", settings)
	}
}
