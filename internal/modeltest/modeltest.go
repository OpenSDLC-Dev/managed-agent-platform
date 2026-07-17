// Package modeltest is test support for the tiers that call a real model
// endpoint: the provider live-contract tests and the end-to-end eval suite.
// It owns the opt-in contract those tiers share — consent to spend money is a
// tier variable in the environment, never the presence of a configured .env —
// and resolves the one endpoint they drive. Production code must never import
// it.
//
// The tiers, and what each costs:
//
//	RUN_LIVE_MODEL_TESTS=1  one real turn against the configured endpoint (cents)
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

// modelPrefix is the only key prefix read from .env. The filter is what makes
// the file structurally incapable of opting a tier in: a RUN_EVALS=1 line in
// it is not configuration this package can see, whatever it says.
const modelPrefix = "MODEL_"

// knownProtocols are the protocols the platform's provider registry can route.
// An endpoint outside this set is a misconfiguration (a typo, most likely), not
// another adapter's endpoint — the distinction is what keeps a mistyped
// MODEL_PROTOCOL from masquerading as a protocol-mismatch skip. Keep in step
// with the factories cmd/brain registers.
var knownProtocols = []string{"anthropic", "openai"}

// Config is the model endpoint a live tier drives, read from MODEL_PROTOCOL /
// MODEL_BASE_URL / MODEL_API_KEY / MODEL_ID. It mirrors the four fields a
// provider.Config needs.
type Config struct {
	Protocol string // "anthropic" | "openai"
	BaseURL  string
	APIKey   string
	Model    string
}

// String redacts the credential. Printing a Config is the natural first move
// when a live turn misbehaves, so the redaction is a property of the type
// rather than a warning in a comment that a %v somewhere else would ignore.
func (c Config) String() string {
	key := "unset"
	if c.APIKey != "" {
		key = "[redacted]"
	}
	return fmt.Sprintf("{Protocol:%s BaseURL:%s APIKey:%s Model:%s}", c.Protocol, c.BaseURL, key, c.Model)
}

// Format carries the redaction to every verb fmt will let it. String alone
// would not: fmt consults it for %v/%s and their kin, but reaches past it for
// %#v, and for a mismatched verb like %d it falls back to a diagnostic that
// prints the raw fields — precisely the debugging accident this type exists to
// survive. Unexporting the credential would not help either; fmt prints
// unexported fields too.
//
// %p is the one that gets through: fmt resolves %p and %T before consulting
// any method ("we always do them first" — fmt/print.go, printArg), so %p on a
// Config prints a %!p diagnostic carrying the fields. Nothing here can
// intercept it; pointing %p at a struct value is already a mistake fmt shouts
// about, so it is documented rather than defended against.
func (c Config) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		io.WriteString(f, "modeltest.Config"+c.String())
		return
	}
	io.WriteString(f, c.String())
}

// Endpoint gates a live tier and returns the endpoint it should drive.
//
// Not opted in: the test skips, no model is called, and the credential file is
// never even opened — an ordinary `go test ./...` costs nothing and touches
// nothing, whatever the .env holds. Opted in but misconfigured: the test FAILS,
// because a safety net that skips itself when its credentials rot is not a
// safety net. Gate before any `testing.Short()` check, or short mode becomes a
// way to opt in and still not be told the configuration is broken.
//
// Naming protocols restricts the test to an endpoint speaking one of them,
// skipping otherwise: one .env holds one endpoint, and the adapter it does not
// belong to has nothing to prove against it. Name none to accept any endpoint
// the registry can route.
func Endpoint(t *testing.T, tierEnv string, protocols ...string) Config {
	t.Helper()
	cfg, skip, err := endpoint(resolve, tierEnv, protocols)
	if skip != "" {
		t.Skip(skip)
	}
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TierEnabled reports whether tierEnv opts into its tier, for the one caller
// that cannot use Endpoint: a TestMain, which has no *testing.T to skip.
//
// It exists so that caller asks the same question Endpoint asks instead of
// re-spelling the rule — two copies of "what counts as opted in" drift, and the
// drift is silent in the worst direction: a TestMain that reads "off" while
// Endpoint reads "on" skips the suite's setup and then runs its tests against
// the setup that never happened.
//
// A TestMain needs this because expensive setup (a database container, an image
// pull) runs before m.Run and so before any test can skip itself. Tests
// themselves must still call Endpoint — this only answers whether to build the
// world, never whether a given test may run.
func TierEnabled(tierEnv string) bool { return tierEnabled(resolve, tierEnv) }

// tierEnabled is the single spelling of the rule, shared with endpoint below.
// Any non-empty value opts in: naming the variable at all is the consent, and
// second-guessing the value ("0" and "false" look like off) would make the
// contract depend on a convention nobody stated.
func tierEnabled(getenv func(string) string, tierEnv string) bool {
	return getenv(tierEnv) != ""
}

func endpoint(getenv func(string) string, tierEnv string, protocols []string) (Config, string, error) {
	if !tierEnabled(getenv, tierEnv) {
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

// resolve reads one key for the production gate.
func resolve(key string) string { return lookup(os.LookupEnv, dotEnv, key) }

// lookup resolves a key from the environment, falling back to the repo-root
// .env for MODEL_* only. The environment always wins — including when it sets a
// key to the empty string, which is an answer ("this is unset") and not an
// invitation for the file to supply one. Anything outside MODEL_* never reaches
// the file, which is both why a tier variable cannot be opted in from disk and
// why a run that never asks for a MODEL_* key never opens the credential file.
// That rests on the tier variables living outside the MODEL_ namespace: name a
// tier MODEL_SOMETHING and the gate itself would start consulting the file.
func lookup(lookupEnv func(string) (string, bool), file func() map[string]string, key string) string {
	if v, ok := lookupEnv(key); ok {
		return v
	}
	if !strings.HasPrefix(key, modelPrefix) {
		return ""
	}
	return file()[key]
}

// dotEnv parses the repo-root .env once, on first use — the file is gitignored,
// holds live credentials, and only MODEL_* keys are read from it. The values
// are kept here rather than pushed into the process environment with os.Setenv:
// that mutation would outlive the test that triggered it, and once any test
// restored a MODEL_* key to unset (t.Setenv's cleanup does exactly that), the
// already-fired load would never refill it for the next caller. A missing file
// is not an error — the environment may carry everything.
var dotEnv = sync.OnceValue(func() map[string]string {
	f, err := os.Open(filepath.Join(repoRoot(), ".env"))
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseDotEnv(f)
})

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
		if !ok || strings.HasPrefix(line, "#") || !strings.HasPrefix(key, modelPrefix) {
			continue
		}
		out[strings.TrimSpace(key)] = parseValue(value)
	}
	return out
}

// parseValue takes the value side of one .env line. A quoted value is whatever
// the quotes enclose, so a '#' inside them is content and anything after the
// closing quote is not; an unquoted one runs to a '#' that follows whitespace,
// which is a trailing comment, and keeps a '#' that does not — some credentials
// contain one.
func parseValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		if end := strings.IndexByte(s[1:], s[0]); end >= 0 {
			return s[1 : 1+end]
		}
	}
	if i := commentStart(s); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func commentStart(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i
		}
	}
	return -1
}
