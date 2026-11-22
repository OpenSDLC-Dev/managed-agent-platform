package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/tools/harnessdoctor/internal/toml"
)

func TestSessionRequiresWorkflowRoles(t *testing.T) {
	for _, role := range []string{"issue-triage", "reviewer", "verifier"} {
		t.Run(role, func(t *testing.T) {
			root := fixture(t)
			const catalog = "harness/manifest.json"
			var m manifest
			if err := json.Unmarshal([]byte(read(t, root, catalog)), &m); err != nil {
				t.Fatal(err)
			}
			m.Agents = slices.DeleteFunc(m.Agents, func(a agent) bool { return a.Name == role })
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			put(t, root, catalog, string(data))
			if err := os.Remove(filepath.Join(root, "harness", "agents", role+".md")); err != nil {
				t.Fatal(err)
			}
			if got := call(t, root, 1, "sync"); !strings.Contains(got, catalog) || !strings.Contains(got, role) {
				t.Fatal(got)
			}
		})
	}
}

func TestCodexRoleTOMLRoundTrip(t *testing.T) {
	root := fixture(t)
	instructions := "Compare \"two\" paths C:\\path.\nPreserve DEL (\x7f), a tab\tand Unicode \u03bb.\n"
	put(t, root, "harness/agents/reviewer.md", instructions)
	call(t, root, 0, "sync")
	var role map[string]any
	if _, err := toml.Decode(read(t, root, ".codex/agents/reviewer.toml"), &role); err != nil || role["developer_instructions"] != instructions {
		t.Fatalf("TOML role did not round-trip: %#v (%v)", role, err)
	}
}

func TestSkillFrontmatterKeys(t *testing.T) {
	for _, tc := range []struct{ header, problem string }{
		{"'name': example\n\"description\": example", ""},
		{"name: example\ndescription: example\n'model': pinned", "model"},
		{"name: other\n\"name\": example\ndescription: example", "duplicate"},
		{"name: example\ndescription: example\n\"eff\\u006frt\": low", "effort"},
	} {
		err := skillHeader("example", []byte("---\n"+tc.header+"\n---\nWorkflow.\n"))
		if (err == nil) != (tc.problem == "") || err != nil && !strings.Contains(err.Error(), tc.problem) {
			t.Errorf("%s: got %v, wanted %q", tc.header, err, tc.problem)
		}
	}
}

func TestScopedGlobsKeepRoot(t *testing.T) {
	root := fixture(t)
	file := "harness/manifest.json"
	put(t, root, file, strings.Replace(read(t, root, file), "\"src/**\"", "\"Makefile\",\"src/**\"", 1))
	call(t, root, 0, "sync")
	projected := read(t, root, ".agents/rules/source.md")
	if !strings.Contains(projected, "globs: \"Makefile,src/**\"") {
		t.Fatal(projected)
	}
	if rootRule, nested := call(t, root, 0, "rules", "Makefile"), call(t, root, 0, "rules", "examples/Makefile"); !strings.Contains(rootRule, "Source rule.") || nested != "" {
		t.Fatalf("root: %q nested: %q", rootRule, nested)
	}
}

func TestCodexProjectSubagentSwitches(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	const config = ".codex/config.toml"
	for _, tc := range []struct {
		text string
		want int
	}{
		{"[features]\nmulti_agent = false", 1},
		{"[agents]\nenabled = false", 1},
		{"[features]\nmulti_agent = true", 0},
		{"[features]\nmulti_agent_v2 = false", 0},
		{"[features]\nmulti_agent = false\nmulti_agent_v2 = true", 0},
		{"[agents]\nenabled = false\n[features.multi_agent_v2]\nenabled = true", 0},
		{"[features]\nmulti_agent = false\n[features.multi_agent_v2]\nenabled = false", 1},
	} {
		put(t, root, config, tc.text)
		got := call(t, root, tc.want, "check", "--host", "codex")
		if tc.want == 1 && !strings.Contains(got, config) || read(t, root, config) != tc.text {
			t.Fatalf("%q: %s", tc.text, got)
		}
	}
}

func TestRegistryShapesAndAlias(t *testing.T) {
	handler := func() map[string]any {
		return map[string]any{"command": "read", "type": "command"}
	}
	withTimeout := handler()
	withTimeout["timeout"] = json.Number("30")
	if !registeredShape([]any{handler(), withTimeout}, []any{withTimeout, handler()}, "") {
		t.Fatal("reordered registrations lost a valid assignment")
	}
	if !registeredShape([]any{handler()}, []any{withTimeout}, "") || registeredShape([]any{withTimeout, withTimeout}, []any{handler(), withTimeout}, "") {
		t.Fatal("missing canonical fields should be repairable, duplicates must remain conflicts")
	}
	root := fixture(t)
	const src = "harness/hooks/claude.json"
	canonical := read(t, root, src)
	for _, bad := range []string{
		strings.Replace(canonical, "{", "{\"disableAllHooks\":true,", 1),
		strings.ReplaceAll(canonical, "map-doctor", "status"),
		strings.ReplaceAll(canonical, "map-pretool", "status"),
	} {
		if _, err := mergeRegistry("claude", []byte(bad), nil); err == nil {
			t.Fatal("accepted canonical field or a built-in alias")
		}
	}
}

func TestLinkedDirectoryLeadsToPhysicalWorktree(t *testing.T) {
	root := fixture(t)
	dir := filepath.Join(root, "src", "nested")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(dir, linked); err != nil {
		t.Skipf("symlink creation unavailable: %v", err) // hygiene:allow Windows may require Developer Mode or symlink privilege
	}
	if got := call(t, linked, 0, "rules", "io.go"); !strings.Contains(got, "Storage rule.") {
		t.Fatal(got)
	}
}

func TestSyncAcceptsCanonicalTimeoutAndPreservesLaterEdits(t *testing.T) {
	root := fixture(t)
	call(t, root, 0, "sync")
	const source = "harness/hooks/claude.json"
	const settings = ".claude/settings.json"
	put(t, root, source, strings.Replace(read(t, root, source), "\"type\": \"command\"", "\"timeout\": 30, \"type\": \"command\"", 1))
	m, err := loadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := render(root, "claude", m)
	if err != nil {
		t.Fatal(err)
	}
	changes, conflicts, _ := compare(root, "claude", m, files)
	if len(changes) != 1 || changes[0].name != settings || len(conflicts) != 0 {
		t.Fatalf("expected one managed registry update: %v, %v", changes, conflicts)
	}
	// Change a local setting after the merge was computed.
	before := read(t, root, settings)
	updated := strings.Replace(before, "{", "{\"user-setting\": true,", 1)
	put(t, root, settings, updated)
	if err := changes[0].apply(root); err == nil || !strings.Contains(err.Error(), "changed during sync") || read(t, root, settings) != updated {
		t.Fatalf("concurrent edit lost: %v", err)
	}
	call(t, root, 0, "sync")
	after := read(t, root, settings)
	if !strings.Contains(after, "\"timeout\": 30") || !strings.Contains(after, "\"user-setting\": true") {
		t.Fatal(after)
	}
	if samePreimage(nil, []byte{}) || samePreimage([]byte{}, nil) {
		t.Fatal("creation of an empty file must count as an edit")
	}
}
