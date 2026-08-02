package gcpkmstest

import (
	"strings"
	"testing"
)

// The consent rule, tested on the rule function rather than through the
// environment, so a machine that happens to have RUN_LIVE_KMS_TESTS set cannot
// change the answer. The shape mirrors webtooltest's own test of the same rule:
// consent comes only from the environment, configuration may come from .env,
// and opting in without configuration FAILS rather than skips — a safety net
// that quietly skips itself when its configuration rots is not a safety net.
func TestLiveKeyNameRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		wantName string
		wantSkip bool
		wantErr  bool
	}{
		{
			name:     "not opted in",
			env:      map[string]string{},
			wantSkip: true,
		},
		{
			name:     "not opted in, even with a key name configured",
			env:      map[string]string{KeyNameEnv: "projects/p/locations/l/keyRings/r/cryptoKeys/k"},
			wantSkip: true,
		},
		{
			name:    "opted in with no key name fails",
			env:     map[string]string{LiveEnv: "1"},
			wantErr: true,
		},
		{
			name: "opted in and configured",
			env: map[string]string{
				LiveEnv:    "1",
				KeyNameEnv: "projects/p/locations/l/keyRings/r/cryptoKeys/k",
			},
			wantName: "projects/p/locations/l/keyRings/r/cryptoKeys/k",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, skip, err := liveKeyName(func(k string) string { return tc.env[k] })
			if (skip != "") != tc.wantSkip {
				t.Errorf("skip = %q, wantSkip %v", skip, tc.wantSkip)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
		})
	}
}

// The environment always wins over the file — including when it sets the key to
// the empty string, which is an answer ("this is unset") and not an invitation
// for the file to supply one. And nothing but the key name is ever read from
// the file, so consent can never come from disk.
func TestLookupPrefersTheEnvironment(t *testing.T) {
	file := func() map[string]string {
		return map[string]string{KeyNameEnv: "from-file", LiveEnv: "1"}
	}
	env := func(vals map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := vals[k]; return v, ok }
	}

	if got := lookup(env(map[string]string{KeyNameEnv: "from-env"}), file, KeyNameEnv); got != "from-env" {
		t.Errorf("with both set, lookup = %q, want the environment's value", got)
	}
	if got := lookup(env(map[string]string{KeyNameEnv: ""}), file, KeyNameEnv); got != "" {
		t.Errorf("an empty environment value must win, got %q from the file", got)
	}
	if got := lookup(env(nil), file, KeyNameEnv); got != "from-file" {
		t.Errorf("unset in the environment should fall back to the file, got %q", got)
	}
	// The tier variable lives outside the keys the file may answer for.
	if got := lookup(env(nil), file, LiveEnv); got != "" {
		t.Errorf("consent must never come from the file, got %q", got)
	}
}

// The .env dialect, read exactly as modeltest and webtooltest read the same
// file: quoted values keep an inner '#', unquoted ones stop at a '#' that
// follows whitespace and keep one that does not, and only the KMS key is ever
// taken from the file.
func TestParseValue(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`plain`, "plain"},
		{`  padded  `, "padded"},
		{`"quoted # with a hash"`, "quoted # with a hash"},
		{`'single quoted'`, "single quoted"},
		{`bare # trailing comment`, "bare"},
		{`has#hash`, "has#hash"},
		{``, ""},
	} {
		if got := parseValue(tc.in); got != tc.want {
			t.Errorf("parseValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseDotEnvTakesOnlyTheKeyName(t *testing.T) {
	got := parseDotEnv(strings.NewReader(
		"# a comment\n" +
			"MODEL_API_KEY=sk-should-not-be-read\n" +
			KeyNameEnv + "=projects/p/locations/l/keyRings/r/cryptoKeys/k\n" +
			LiveEnv + "=1\n" +
			"malformed line with no equals\n"))
	if len(got) != 1 {
		t.Fatalf("parseDotEnv read %d keys, want exactly the key name: %v", len(got), got)
	}
	if got[KeyNameEnv] != "projects/p/locations/l/keyRings/r/cryptoKeys/k" {
		t.Errorf("%s = %q", KeyNameEnv, got[KeyNameEnv])
	}
}
