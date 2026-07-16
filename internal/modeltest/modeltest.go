// Package modeltest is test support for the tiers that call a real model
// endpoint: the provider live-contract tests and the end-to-end eval suite.
// It owns the opt-in contract those tiers share — consent to spend money is a
// tier variable in the environment, never the presence of a configured .env —
// and resolves the one endpoint they drive. Production code must never import
// it.
//
// The tiers, and what each costs:
//
//	RUN_LIVE_MODEL_TESTS=1  one text turn per provider adapter (cents)
//	RUN_EVALS=1             the end-to-end eval suite (minutes, dollars)
//
// Two variables rather than one because their costs differ by an order of
// magnitude: opting into the cheap smoke must not silently buy the suite.
package modeltest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The tier opt-in variables. Any non-empty value opts in.
const (
	// LiveEnv opts into the live-model contract tier: the provider adapters'
	// single real turn against the configured endpoint.
	LiveEnv = "RUN_LIVE_MODEL_TESTS"
	// EvalsEnv opts into the live-system eval suite: whole sessions driven
	// through the public API against a real model and real sandboxes.
	EvalsEnv = "RUN_EVALS"
)

// protocols the platform's provider registry can route. An endpoint outside
// this set is a misconfiguration, not another adapter's endpoint.
var knownProtocols = []string{"anthropic", "openai"}

// Config is the model endpoint a live tier drives, read from MODEL_PROTOCOL /
// MODEL_BASE_URL / MODEL_API_KEY / MODEL_ID. It mirrors the four fields a
// provider.Config needs; APIKey is never logged.
type Config struct {
	Protocol string // "anthropic" | "openai"
	BaseURL  string
	APIKey   string
	Model    string
}

// Enabled reports whether tierEnv opts into its live tier. Tests call
// Endpoint, which gates on this itself; TestMain calls Enabled directly to
// decide whether the tier's fixtures — a Postgres container, a pulled sandbox
// image — are worth building at all.
func Enabled(tierEnv string) bool { return os.Getenv(tierEnv) != "" }

// Endpoint gates a live tier and returns the endpoint it should drive.
//
// Not opted in: the test skips, and no model is called — an ordinary
// `go test ./...` costs nothing, even with a fully populated .env. Opted in
// but misconfigured: the test FAILS, because a safety net that skips itself
// when its credentials rot is not a safety net.
//
// Naming protocols restricts the test to an endpoint speaking one of them,
// skipping otherwise: one .env holds one endpoint, and the adapter it does not
// belong to has nothing to prove against it. Name none to accept any endpoint
// the registry can route.
func Endpoint(t *testing.T, tierEnv string, protocols ...string) Config {
	t.Helper()
	LoadDotEnv()
	cfg, skip, err := endpoint(os.Getenv, tierEnv, protocols)
	if skip != "" {
		t.Skip(skip)
	}
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func endpoint(getenv func(string) string, tierEnv string, protocols []string) (Config, string, error) {
	if getenv(tierEnv) == "" {
		return Config{}, fmt.Sprintf("%s is not set: skipping the live tier (no model is called)", tierEnv), nil
	}
	cfg := Config{
		Protocol: getenv("MODEL_PROTOCOL"),
		BaseURL:  getenv("MODEL_BASE_URL"),
		APIKey:   getenv("MODEL_API_KEY"),
		Model:    getenv("MODEL_ID"),
	}
	var missing []string
	for _, key := range []struct {
		name  string
		value string
	}{
		{"MODEL_PROTOCOL", cfg.Protocol},
		{"MODEL_BASE_URL", cfg.BaseURL},
		{"MODEL_API_KEY", cfg.APIKey},
		{"MODEL_ID", cfg.Model},
	} {
		if key.value == "" {
			missing = append(missing, key.name)
		}
	}
	if len(missing) > 0 {
		return Config{}, "", fmt.Errorf("%s opted into the live tier but %s %s unset: "+
			"set them in the environment or the repo-root .env",
			tierEnv, strings.Join(missing, ", "), plural(len(missing), "is", "are"))
	}
	if !slices.Contains(knownProtocols, cfg.Protocol) {
		return Config{}, "", fmt.Errorf("MODEL_PROTOCOL=%q is not a protocol this platform speaks (%s)",
			cfg.Protocol, strings.Join(knownProtocols, ", "))
	}
	if len(protocols) > 0 && !slices.Contains(protocols, cfg.Protocol) {
		return Config{}, fmt.Sprintf("the configured endpoint speaks %s; this test drives %s",
			cfg.Protocol, strings.Join(protocols, "/")), nil
	}
	return cfg, "", nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

var loadOnce sync.Once

// LoadDotEnv fills the MODEL_* variables from the repo-root .env when they are
// unset, so a developer configures the endpoint once instead of exporting four
// variables per shell. The environment always wins over the file, and the file
// never opts a tier in: it supplies configuration, the tier variable supplies
// consent. A missing .env is not an error — the environment may carry
// everything. Only MODEL_* keys are read, and no value is ever printed.
func LoadDotEnv() {
	loadOnce.Do(func() {
		f, err := os.Open(filepath.Join(repoRoot(), ".env"))
		if err != nil {
			return
		}
		defer f.Close()
		for key, value := range parseDotEnv(f) {
			if os.Getenv(key) == "" {
				os.Setenv(key, value)
			}
		}
	})
}

// repoRoot derives the checkout root from this file's compile-time path, so
// every caller reaches the same .env regardless of its own package directory.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func parseDotEnv(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") || !strings.HasPrefix(key, "MODEL_") {
			continue
		}
		value = strings.TrimSpace(value)
		// A quoted value is unwrapped as-is; an unquoted one loses any
		// trailing inline comment.
		if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		out[strings.TrimSpace(key)] = value
	}
	return out
}
