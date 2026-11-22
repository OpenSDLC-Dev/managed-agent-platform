package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNativeInputsAndRoleBoundaries(t *testing.T) {
	cases := []struct{ host, payload, wantTool, command, dir, role string }{
		{"claude", `{"tool_name":"Bash","cwd":"/src","tool_input":{"command":"git status"}}`, "Bash", "git status", "/src", ""},
		{"codex", `{"agent_type":"issue-triage","tool_name":"apply_patch","cwd":"/work","tool_input":{"command":"text"}}`, "apply_patch", "text", "/work", "issue-triage"},
		{"codex", `{"tool_name":"Bash","cwd":"/src","tool_input":{"command":"git status","workdir":"nested"}}`, "Bash", "git status", filepath.Join("/src", "nested"), ""},
		{"antigravity", `{"toolCall":{"name":"run_command","args":{"CommandLine":"git commit","Cwd":"/work"}},"conversationId":"abc"}`, "run_command", "git commit", "", ""},
		{"antigravity", `{"cwd":"/work","toolCall":{"name":"run_command","args":{"CommandLine":"git commit"}}}`, "run_command", "git commit", "/work", ""},
		{"antigravity", `{"cwd":"/work","toolCall":{"name":"run_command","args":{"CommandLine":"git commit","Cwd":"nested"}}}`, "run_command", "git commit", "/work", ""},
	}
	for _, c := range cases {
		var in input
		if err := json.Unmarshal([]byte(c.payload), &in); err != nil {
			t.Fatal(err)
		}
		got := in.event(c.host)
		if got.tool != c.wantTool || got.command != c.command || got.dir != c.dir || got.role != c.role {
			t.Errorf("%s: %#v", c.host, got)
		}
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "harness"), 0755); err != nil {
		t.Fatal(err)
	}
	const roles = `{"agents":[{"name":"custom-reader","codexEvidenceOnly":true},{"name":"reviewer","codexEvidenceOnly":true},{"name":"issue-triage","codexEvidenceOnly":true},{"name":"verifier"}]}`
	if err := os.WriteFile(filepath.Join(root, "harness", "manifest.json"), []byte(roles), 0644); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"issue-triage", "reviewer", "custom-reader"} {
		for _, tool := range []string{"apply_patch", "Bash", "spawn_agent", "mcp__service__update", "exec_command"} {
			var out bytes.Buffer
			err := handle("codex", event{role: role, tool: tool}, root, &out, &bytes.Buffer{})
			if err != nil || !strings.Contains(out.String(), `"permissionDecision":"deny"`) {
				t.Fatalf("%s/%s decision %s, %v", role, tool, &out, err)
			}
		}
	}
	// A registered adapter can outlive its catalog entry until sync is repaired.
	dir := filepath.Join(root, ".codex", "agents")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orphan.toml"), []byte("name = 'orphan'\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "harness", "manifest.json"), []byte(`{"agents":[]}`), 0644); err != nil {
		t.Fatal(err)
	}
	for role, restricted := range map[string]bool{"orphan": true, "reviewer": true, "issue-triage": true, "independent": false} {
		var out bytes.Buffer
		if err := handle("codex", event{role: role, tool: "list_tools"}, root, &out, &bytes.Buffer{}); err != nil ||
			strings.Contains(out.String(), `"permissionDecision":"deny"`) != restricted {
			t.Fatalf("%s: %s, %v", role, &out, err)
		}
	}
}

func TestCommandsAreScopedToTheirWorktree(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "one space")
	tests := []struct {
		command string
		targets []commit
	}{
		{"git status", nil},
		{`echo "git commit"`, nil},
		{"git add . && git commit -m ok", []commit{{root, false}}},
		{"git add x\ngit commit -m 'fix\n\nVerified-By: make verify'", []commit{{root, true}}},
		{`git -C "one space" commit --trailer "Verified-By: none"`, []commit{{nested, true}}},
		{`Set-Location -LiteralPath "one space"; git.exe commit -m ok`, []commit{{nested, false}}},
		{`git -C "one space" commit -m first && git commit -m second`, []commit{{nested, false}, {root, false}}},
		{`git -C"one space" commit -m first && git commit -m second`, []commit{{nested, false}, {root, false}}},
		{`git -C=other commit -m first`, []commit{{filepath.Join(root, "=other"), false}}},
		{`GIT_INDEX_FILE=foreign git commit -m sample; git commit -m next`, []commit{{"", false}, {root, false}}},
		{`GIT_DIR=other/.git git -C "one space" commit --trailer "Verified-By: manual"`, []commit{{"", true}}},
		{`git --work-tree /elsewhere -C "one space" commit -m ok`, []commit{{"", false}}},
		{`GIT_WORK_TREE=foreign sh -c 'git commit -m sample'`, []commit{{"", false}}},
		{`LANG=C git commit -m sample`, []commit{{root, false}}},
		{`git init "one space" && git -C "one space" commit -m first`, []commit{{"", false}}},
		{`git -C "one space" init && git -C "one space" commit --trailer "Verified-By: manual"`, []commit{{"", true}}},
		{`git worktree add -b topic "one space" HEAD && git -C "one space" commit -m first`, []commit{{"", false}}},
		{`sh -c 'git init "one space"' && git -C "one space/subdir" commit -m first`, []commit{{"", false}}},
		{`git init other && git -C "one space" commit -m unrelated`, []commit{{nested, false}}},
		{`git clone origin "one space" && git -C "one space" commit -m first`, []commit{{"", false}}},
		{`git clone origin "one space" && git commit -m unrelated`, []commit{{root, false}}},
		{`git clone ../other/source.git && git -C source commit -m first`, []commit{{"", false}}},
		{`git commit -m first && echo "Verified-By: other"`, []commit{{root, false}}},
		{`(cd "one space" && git status); git commit -m root`, []commit{{root, false}}},
		{`(cd "one space" && (git commit -m child)); git commit -m root`, []commit{{nested, false}, {root, false}}},
		{`echo "(cd somewhere)"; git commit -m root`, []commit{{root, false}}},
	}
	for _, tc := range tests {
		got := commits(tc.command, root)
		if !reflect.DeepEqual(got, tc.targets) {
			t.Errorf("%q => %#v, want %#v", tc.command, got, tc.targets)
		}
	}
}

func TestCommitMessageOptionForms(t *testing.T) {
	message := "fix\n\nVerified-By: manual check"
	for _, args := range [][]string{{"-am", message}, {"-m" + message}, {"-qsm" + message}, {"--message=" + message}} {
		if !hasTrailer(args, false) {
			t.Errorf("missed trailer for %q", args)
		}
	}
	for _, args := range [][]string{{"--", "-m", message}, {"-F" + message}, {"-Smy-key", message}} {
		if hasTrailer(args, false) {
			t.Errorf("treated non-message argument as a trailer: %q", args)
		}
	}
}

func TestLiteralCommitMessageOnStdin(t *testing.T) {
	root := t.TempDir()
	const text = " <<'MSG'\nsubject\n\nVerified-By: manual check\nMSG\n"
	for _, tc := range []struct {
		command string
		targets []commit
	}{
		{"git commit -F -" + text, []commit{{root, true}}},
		{"git commit -F -" + strings.TrimLeft(text, " "), []commit{{root, true}}},
		{"git commit -F - 00" + strings.TrimSpace(text), []commit{{root, true}}},
		{"git commit -F - 0" + strings.TrimSpace(text), []commit{{root, true}}},
		{"git commit -F - 3" + strings.TrimSpace(text), []commit{{root, false}}},
		{"git commit -F - <" + strings.TrimSpace(text), []commit{{root, false}}},
		{"git commit --file=-" + text + "git commit -m next", []commit{{root, true}, {root, false}}},
		{"cat" + text + "git commit -F -", []commit{{root, false}}},
		{"git commit -m subject" + text, []commit{{root, false}}},
		{"git commit -m -F-" + text, []commit{{root, false}}},
		{"git commit --message --file=-" + text, []commit{{root, false}}},
		{"git commit -F - -F other-file" + text, []commit{{root, false}}},
		{"git commit -F - <<'MSG'\nno trailer\nMSG\n", []commit{{root, false}}},
	} {
		if got := commits(tc.command, root); !reflect.DeepEqual(got, tc.targets) {
			t.Errorf("%q: got %v want %v", tc.command, got, tc.targets)
		}
	}
}

func TestHookProtocol(t *testing.T) {
	for _, host := range []string{"claude", "codex", "antigravity"} {
		var out bytes.Buffer
		if err := decision(host, "", &out); err != nil || out.Len() != 0 {
			t.Fatalf("%s allowed: %q %v", host, &out, err)
		}
		if err := decision(host, "needs verification", &out); err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if host == "antigravity" && doc["decision"] != "deny" {
			t.Fatalf("Antigravity response %v", doc)
		}
		if host != "antigravity" && !strings.Contains(out.String(), `"permissionDecision":"deny"`) {
			t.Fatalf("%s response %v", host, doc)
		}
	}
}

func TestEmptyCommandDirectoryCannotBorrowWorkingTreeStamp(t *testing.T) {
	var out, diagnostic bytes.Buffer
	t.Setenv("VERIFY_GATE_MODE", "enforce")
	t.Setenv("VERIFY_GATE_STATE_FILE", filepath.Join(t.TempDir(), "blocks"))
	if err := handle("codex", event{tool: "Bash", command: "git commit -m test"}, filepath.Join(mustGetwd(t), "../.."), &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "deny") {
		t.Fatalf("missing target was allowed: %s %s", &out, &diagnostic)
	}
}

func TestFutureCommitDirectoryUsesGateModeAndTrailer(t *testing.T) {
	sourceRoot := filepath.Join(mustGetwd(t), "../..")
	root := t.TempDir()
	t.Setenv("VERIFY_GATE_STATE_FILE", filepath.Join(root, "blocks"))
	for _, tc := range []struct {
		mode, command, want string
	}{
		{"observe", "git -C future commit -m wip", "observe"},
		{"enforce", "git -C future commit -m wip", "deny"},
		{"enforce", "git -C future commit -m wip --trailer 'Verified-By: explicit'", ""},
	} {
		t.Setenv("VERIFY_GATE_MODE", tc.mode)
		var out, log bytes.Buffer
		if err := handle("claude", event{tool: "Bash", command: tc.command, dir: root}, sourceRoot, &out, &log); err != nil {
			t.Fatal(err)
		}
		if s := out.String() + log.String(); (tc.want == "" && s != "") || !strings.Contains(s, tc.want) {
			t.Errorf("%s %s: %q", tc.mode, tc.command, s)
		}
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
