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
		{"antigravity", `{"toolCall":{"name":"run_command","args":{"CommandLine":"git commit","Cwd":"/work"}},"conversationId":"abc"}`, "run_command", "git commit", "/work", ""},
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
	for _, role := range []string{"issue-triage", "reviewer"} {
		for _, tool := range []string{"apply_patch", "Bash", "spawn_agent", "mcp__service__update", "exec_command"} {
			var out bytes.Buffer
			err := handle("codex", event{role: role, tool: tool}, ".", &out, &bytes.Buffer{})
			if err != nil || !strings.Contains(out.String(), `"permissionDecision":"deny"`) {
				t.Fatalf("%s/%s decision %s, %v", role, tool, &out, err)
			}
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
		{`git commit -m first && echo "Verified-By: other"`, []commit{{root, false}}},
	}
	for _, tc := range tests {
		got := commits(tc.command, root)
		if !reflect.DeepEqual(got, tc.targets) {
			t.Errorf("%q => %#v, want %#v", tc.command, got, tc.targets)
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

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
