package main

import (
	"strconv"
	"strings"
	"testing"
)

// Keep both required callbacks in the document: a malformed auxiliary handler
// can make Codex discard the entire project registry.
func withOptionalCodexCommand(t *testing.T, base, options string) string {
	t.Helper()
	doc, err := jsonObject([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	optional, err := jsonObject([]byte(options))
	if err != nil {
		t.Fatal(err)
	}
	optional["type"] = "command"
	optional["command"] = "echo optional"
	group := doc["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)
	group["hooks"] = append(group["hooks"].([]any), optional)
	b, err := formattedJSON(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOptionalCodexCommandSchema(t *testing.T) {
	root := fixture(t)
	base := read(t, root, "harness/hooks/codex.json")
	for _, tc := range []struct{ options, problem string }{
		{`{}`, ""},
		{`{"async":true,"command_windows":"echo alternate","timeout":19,"statusMessage":"Working","additionalContextLimit":23}`, ""},
		{`{"commandWindows":null,"statusMessage":null,"timeout":null,"additionalContextLimit":null}`, ""},
		{`{"timeout":0,"additionalContextLimit":0}`, ""},
		{`{"timeout":9223372036854775807}`, ""},
		{`{"timeout":9223372036854775808}`, "timeout"},
		{`{"additionalContextLimit":9223372036854775808}`, "additionalContextLimit"},
		{`{"async":"false"}`, "async"},
		{`{"async":null}`, "async"},
		{`{"commandWindows":false}`, "commandWindows"},
		{`{"command_windows":false}`, "command_windows"},
		{`{"commandWindows":"echo same","command_windows":"echo same"}`, "command_windows"},
		{`{"statusMessage":false}`, "statusMessage"},
		{`{"timeout":"19"}`, "timeout"},
		{`{"timeout":-1}`, "timeout"},
		{`{"timeout":1.0}`, "timeout"},
		{`{"timeout":1e1}`, "timeout"},
		{`{"timeout":18446744073709551616}`, "timeout"},
		{`{"additionalContextLimit":-1}`, "additionalContextLimit"},
		{`{"additionalContextLimit":0.5}`, "additionalContextLimit"},
		{`{"additionalContextLimit":18446744073709551616}`, "additionalContextLimit"},
	} {
		doc, err := jsonObject([]byte(withOptionalCodexCommand(t, base, tc.options)))
		if err != nil {
			t.Fatal(err)
		}
		for _, canonical := range []bool{false, true} {
			err := validateRegistry("codex", doc, canonical)
			if (err == nil) != (tc.problem == "") || err != nil && !strings.Contains(err.Error(), tc.problem) {
				t.Errorf("canonical=%t %s: %v, want %q", canonical, tc.options, err, tc.problem)
			}
		}
	}
}

func TestCodexNormalizesUnusedLimitsBeforeRegistration(t *testing.T) {
	for _, tc := range []struct{ event, fields string }{
		{"SessionEnd", `{"timeout":18446744073709551615}`},
		{"Stop", `{"additionalContextLimit":` + strconv.FormatUint(uint64(^uint(0)), 10) + "}"},
	} {
		fields, err := jsonObject([]byte(tc.fields))
		if err != nil {
			t.Fatal(err)
		}
		if err := codexCommandFields(tc.event, fields); err != nil {
			t.Fatalf("%s: %v", tc.event, err)
		}
	}
}

func TestMalformedOptionalCodexHookPreservesBothRegistries(t *testing.T) {
	const source, generated = "harness/hooks/codex.json", ".codex/hooks.json"
	root := fixture(t)
	base := read(t, root, source)
	registered := withOptionalCodexCommand(t, base, `{"async":true,"timeout":12,"commandWindows":"echo optional windows"}`)
	put(t, root, source, registered)
	call(t, root, 0, "sync")
	call(t, root, 0, "check", "--host", "codex")
	if got := call(t, root, 0, "session-start", "codex"); !strings.Contains(got, "harness-doctor: OK") {
		t.Fatal(got)
	}
	originalGenerated := read(t, root, generated)
	for _, tc := range []struct{ file, options, field string }{
		{source, `{"async":"false"}`, "async"},
		{generated, `{"async":true,"timeout":12.5,"commandWindows":"echo optional windows"}`, "timeout"},
	} {
		put(t, root, tc.file, withOptionalCodexCommand(t, base, tc.options))
		beforeSource, beforeGenerated := read(t, root, source), read(t, root, generated)
		for _, invocation := range [][]string{{"check"}, {"sync"}, {"session-start", "codex"}} {
			status := 1
			if invocation[0] == "session-start" {
				status = 0
			}
			got := call(t, root, status, invocation...)
			if !strings.Contains(got, "harness-doctor: FAIL") || !strings.Contains(got, tc.file) || !strings.Contains(got, tc.field) {
				t.Errorf("%v: %s", invocation, got)
			}
			if read(t, root, source) != beforeSource || read(t, root, generated) != beforeGenerated {
				t.Fatalf("%v overwrote a registry", invocation)
			}
		}
		put(t, root, source, registered)
		put(t, root, generated, originalGenerated)
	}
}
