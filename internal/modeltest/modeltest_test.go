package modeltest

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseDotEnvReadsOnlyModelKeys(t *testing.T) {
	const file = `# a comment
MODEL_PROTOCOL=anthropic
MODEL_BASE_URL="https://gateway.example/api"
MODEL_API_KEY='sk-single-quoted'
MODEL_ID=some-model # the snapshot we pin

# MODEL_ID=commented-out
OTHER_KEY=ignored
DATABASE_URL=postgres://nope
no-equals-sign
MODEL_EMPTY=
`
	got := parseDotEnv(strings.NewReader(file))
	want := map[string]string{
		"MODEL_PROTOCOL": "anthropic",
		"MODEL_BASE_URL": "https://gateway.example/api",
		"MODEL_API_KEY":  "sk-single-quoted",
		"MODEL_ID":       "some-model",
		"MODEL_EMPTY":    "",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d keys, want %d: %v", len(got), len(want), keysOf(got))
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

// A .env is hand-written, so its comments and quotes are whatever a human
// typed. Every case here is one a real file produces and an earlier version of
// this parser got wrong — silently, by handing the endpoint a value with a
// comment welded onto it.
func TestParseValue(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", `some-model`, "some-model"},
		{"space-delimited comment", `some-model # pinned`, "some-model"},
		{"tab-delimited comment", "some-model\t# pinned", "some-model"},
		{"hash inside the value", `sk-abc#def`, "sk-abc#def"},
		{"double-quoted", `"https://gateway.example/api"`, "https://gateway.example/api"},
		{"single-quoted", `'sk-secret'`, "sk-secret"},
		{"quoted, then a comment", `"abc" # note`, "abc"},
		{"hash inside quotes", `"model # pinned"`, "model # pinned"},
		{"hash inside quotes, then a comment", `"model # pinned" # note`, "model # pinned"},
		{"unbalanced quote", `"abc`, `"abc`},
		{"empty", ``, ""},
		{"surrounding space", `  spaced  `, "spaced"},
	} {
		if got := parseValue(tc.in); got != tc.want {
			t.Errorf("%s: parseValue(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestLookupPrefersTheEnvironment(t *testing.T) {
	file := staticFile(map[string]string{"MODEL_ID": "from-file"})
	env := lookupFrom(map[string]string{"MODEL_ID": "from-env"})
	if got := lookup(env, file, "MODEL_ID"); got != "from-env" {
		t.Errorf("MODEL_ID = %q, want the environment to win over the file", got)
	}
}

func TestLookupFallsBackToTheFile(t *testing.T) {
	file := staticFile(map[string]string{"MODEL_ID": "from-file"})
	if got := lookup(lookupFrom(nil), file, "MODEL_ID"); got != "from-file" {
		t.Errorf("MODEL_ID = %q, want the file to supply an unset key", got)
	}
}

// An explicitly empty MODEL_API_KEY means "I am unsetting this", which the gate
// must report as missing. Treating it as absent would let the .env quietly
// refill it and spend money the operator was trying to stop.
func TestLookupTreatsAnExplicitEmptyValueAsTheAnswer(t *testing.T) {
	file := staticFile(map[string]string{"MODEL_API_KEY": "sk-from-file"})
	env := lookupFrom(map[string]string{"MODEL_API_KEY": ""})
	if got := lookup(env, file, "MODEL_API_KEY"); got != "" {
		t.Errorf("MODEL_API_KEY = %q, want an explicitly emptied variable to stay empty", got)
	}
}

// The two halves of "the file supplies configuration, never consent": a tier
// variable never reads the file, so the file cannot opt anyone in — and a run
// that asks only about tier variables never opens the credential file at all.
func TestLookupNeverReadsTheFileForATierVariable(t *testing.T) {
	opened := false
	file := func() map[string]string {
		opened = true
		return map[string]string{EvalsEnv: "1", LiveEnv: "1"}
	}
	if got := lookup(lookupFrom(nil), file, EvalsEnv); got != "" {
		t.Errorf("%s = %q, want the file to be unable to opt a tier in", EvalsEnv, got)
	}
	if opened {
		t.Errorf("resolving %s opened the credential file", EvalsEnv)
	}
}

func TestEndpointSkipsWhenNotOptedIn(t *testing.T) {
	// A fully configured .env is consent to *how* the tier runs, never consent
	// to run it: an ordinary `go test ./...` must call no model.
	cfg, skip, err := endpoint(getenvFrom(fullEnv(map[string]string{LiveEnv: ""})), LiveEnv, nil)
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if skip == "" {
		t.Fatal("a complete .env with no opt-in must skip, not run")
	}
	if !strings.Contains(skip, LiveEnv) {
		t.Errorf("skip reason %q does not name %s, so the reader cannot opt in", skip, LiveEnv)
	}
	if cfg != (Config{}) {
		t.Errorf("skipped gate returned a config: %+v", cfg)
	}
}

func TestEndpointReturnsConfigWhenOptedIn(t *testing.T) {
	cfg, skip, err := endpoint(getenvFrom(fullEnv(nil)), LiveEnv, []string{"anthropic"})
	if err != nil || skip != "" {
		t.Fatalf("gate rejected a complete config: skip=%q err=%v", skip, err)
	}
	want := Config{
		Protocol: "anthropic",
		BaseURL:  "https://gateway.example/api",
		APIKey:   secret,
		Model:    "some-model",
	}
	if cfg != want {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}
}

func TestEndpointNamesEveryMissingKey(t *testing.T) {
	env := fullEnv(map[string]string{"MODEL_BASE_URL": "", "MODEL_API_KEY": ""})

	_, skip, err := endpoint(getenvFrom(env), LiveEnv, nil)
	if skip != "" {
		t.Fatalf("an opted-in tier with missing config must fail, not skip (got skip %q)", skip)
	}
	if err == nil {
		t.Fatal("an opted-in tier with missing config must fail")
	}
	for _, key := range []string{"MODEL_BASE_URL", "MODEL_API_KEY"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not name the missing %s", err, key)
		}
	}
}

func TestEndpointRejectsUnknownProtocol(t *testing.T) {
	// A typo must not sneak out as a protocol-mismatch skip: the tier would go
	// quiet exactly when its configuration broke.
	env := fullEnv(map[string]string{"MODEL_PROTOCOL": "anthropci"})
	_, skip, err := endpoint(getenvFrom(env), LiveEnv, []string{"anthropic"})
	if skip != "" {
		t.Fatalf("an invalid protocol must fail, not skip (got skip %q)", skip)
	}
	if err == nil || !strings.Contains(err.Error(), "anthropci") {
		t.Errorf("error = %v, want it to name the unknown protocol", err)
	}
}

func TestEndpointSkipsForAnotherAdaptersEndpoint(t *testing.T) {
	// One .env holds one endpoint. The adapter it does not belong to has
	// nothing to prove against it — that is a skip, not a misconfiguration.
	_, skip, err := endpoint(getenvFrom(fullEnv(nil)), LiveEnv, []string{"openai"})
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if skip == "" {
		t.Fatal("an anthropic endpoint must not run the openai adapter's live test")
	}
	if !strings.Contains(skip, "anthropic") || !strings.Contains(skip, "openai") {
		t.Errorf("skip reason %q should name both the configured and the wanted protocol", skip)
	}
}

func TestEndpointAcceptsAnyProtocolWhenNoneNamed(t *testing.T) {
	// The eval suite drives whatever the registry can route.
	for _, protocol := range knownProtocols {
		env := fullEnv(map[string]string{"MODEL_PROTOCOL": protocol})
		cfg, skip, err := endpoint(getenvFrom(env), EvalsEnv, nil)
		if err != nil || skip != "" {
			t.Errorf("%s: gate rejected a complete config: skip=%q err=%v", protocol, skip, err)
		}
		if cfg.Protocol != protocol {
			t.Errorf("Protocol = %q, want %q", cfg.Protocol, protocol)
		}
	}
}

func TestEndpointNeverLeaksTheKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"not opted in", fullEnv(map[string]string{LiveEnv: ""}), nil},
		{"wrong protocol", fullEnv(nil), []string{"openai"}},
		{"invalid protocol", fullEnv(map[string]string{"MODEL_PROTOCOL": "bogus"}), nil},
		{"incomplete", fullEnv(map[string]string{"MODEL_ID": ""}), nil},
	} {
		_, skip, err := endpoint(getenvFrom(tc.env), LiveEnv, tc.want)
		if strings.Contains(skip, secret) {
			t.Errorf("%s: skip reason leaks the API key", tc.name)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error leaks the API key", tc.name)
		}
	}
}

// Printing a Config is what anyone does first when a live turn misbehaves, so
// the redaction has to survive that, not just the paths this package controls.
func TestConfigFormattingRedactsTheKey(t *testing.T) {
	cfg, _, err := endpoint(getenvFrom(fullEnv(nil)), LiveEnv, nil)
	if err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	for _, format := range []string{"%v", "%+v", "%s"} {
		rendered := fmt.Sprintf(format, cfg)
		if strings.Contains(rendered, secret) {
			t.Errorf("%s of a Config leaks the API key: %s", format, rendered)
		}
		if !strings.Contains(rendered, "some-model") {
			t.Errorf("%s of a Config dropped the model, leaving nothing to debug with: %s", format, rendered)
		}
	}
}

const secret = "sk-do-not-print-me"

// fullEnv is an opted-in, completely configured anthropic endpoint, with
// overrides applied (an empty value deletes the key).
func fullEnv(overrides map[string]string) map[string]string {
	env := map[string]string{
		LiveEnv:          "1",
		EvalsEnv:         "1",
		"MODEL_PROTOCOL": "anthropic",
		"MODEL_BASE_URL": "https://gateway.example/api",
		"MODEL_API_KEY":  secret,
		"MODEL_ID":       "some-model",
	}
	for k, v := range overrides {
		if v == "" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	return env
}

func getenvFrom(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) { v, ok := env[key]; return v, ok }
}

func staticFile(m map[string]string) func() map[string]string {
	return func() map[string]string { return m }
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
