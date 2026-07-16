package modeltest

import (
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

func TestParseDotEnvQuotedValueKeepsHash(t *testing.T) {
	got := parseDotEnv(strings.NewReader(`MODEL_ID="model # not-a-comment"`))
	if got["MODEL_ID"] != "model # not-a-comment" {
		t.Errorf("MODEL_ID = %q, want the hash preserved inside quotes", got["MODEL_ID"])
	}
}

func TestEnabled(t *testing.T) {
	t.Setenv(EvalsEnv, "")
	if Enabled(EvalsEnv) {
		t.Error("an unset tier variable must not enable the tier")
	}
	t.Setenv(EvalsEnv, "1")
	if !Enabled(EvalsEnv) {
		t.Error("a set tier variable must enable the tier")
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
	env := fullEnv(nil)
	delete(env, "MODEL_BASE_URL")
	delete(env, "MODEL_API_KEY")

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
	env := fullEnv(map[string]string{"MODEL_PROTOCOL": "grpc-telepathy"})
	_, skip, err := endpoint(getenvFrom(env), LiveEnv, nil)
	if skip != "" {
		t.Fatalf("an invalid protocol must fail, not skip (got skip %q)", skip)
	}
	if err == nil || !strings.Contains(err.Error(), "grpc-telepathy") {
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
	for _, protocol := range []string{"anthropic", "openai"} {
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

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
