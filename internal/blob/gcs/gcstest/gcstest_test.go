package gcstest

import (
	"strings"
	"testing"
)

// The gate's rules are tested as a table against an injected getenv rather than
// through LiveBucket, so the ambient environment cannot decide the answer
// (gcpkmstest's shape, for the same reason).

func TestLiveBucketRules(t *testing.T) {
	for name, tc := range map[string]struct {
		env        map[string]string
		wantBucket string
		wantSkip   bool
		wantErr    bool
	}{
		"NotOptedIn": {
			// The bucket may even be configured; without consent nothing runs.
			env:      map[string]string{BucketEnv: "map-blob"},
			wantSkip: true,
		},
		"OptedInAndConfigured": {
			env:        map[string]string{LiveEnv: "1", BucketEnv: "map-blob"},
			wantBucket: "map-blob",
		},
		"OptedInButUnconfigured": {
			// Fails rather than skips: a tier that skips itself when its
			// configuration rots is not a safety net.
			env:     map[string]string{LiveEnv: "1"},
			wantErr: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			bucket, skip, err := liveBucket(func(k string) string { return tc.env[k] })
			if (skip != "") != tc.wantSkip {
				t.Errorf("skip = %q, want skip=%v", skip, tc.wantSkip)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error=%v", err, tc.wantErr)
			}
			if bucket != tc.wantBucket {
				t.Errorf("bucket = %q, want %q", bucket, tc.wantBucket)
			}
			if tc.wantErr && err != nil && !strings.Contains(err.Error(), BucketEnv) {
				t.Errorf("error %q does not name the missing variable", err)
			}
		})
	}
}

func TestLookupPrefersTheEnvironment(t *testing.T) {
	file := func() map[string]string { return map[string]string{BucketEnv: "from-file"} }
	env := func(k string) (string, bool) {
		v, ok := map[string]string{BucketEnv: "from-env"}[k]
		return v, ok
	}
	if got := lookup(env, file, BucketEnv); got != "from-env" {
		t.Errorf("lookup = %q, want the environment's value", got)
	}
	// Set-to-empty is an answer, not an invitation for the file to supply one.
	empty := func(k string) (string, bool) { return "", k == BucketEnv }
	if got := lookup(empty, file, BucketEnv); got != "" {
		t.Errorf("lookup with an empty environment value = %q, want it to stand", got)
	}
	// Consent can never come from disk: no key but the bucket is read from it.
	unset := func(string) (string, bool) { return "", false }
	if got := lookup(unset, func() map[string]string { return map[string]string{LiveEnv: "1"} }, LiveEnv); got != "" {
		t.Errorf("lookup answered %q for the consent variable from the file", got)
	}
}

func TestParseDotEnvKeepsOnlyTheBucket(t *testing.T) {
	got := parseDotEnv(strings.NewReader(strings.Join([]string{
		"# a comment",
		"RUN_LIVE_GCS_TESTS=1",
		"MODEL_API_KEY=secret",
		"GCS_BUCKET=map-blob # trailing comment",
		"malformed line",
	}, "\n")))
	if len(got) != 1 || got[BucketEnv] != "map-blob" {
		t.Errorf("parseDotEnv = %v, want only %s=map-blob", got, BucketEnv)
	}
}

func TestParseValue(t *testing.T) {
	for in, want := range map[string]string{
		`  map-blob  `:        "map-blob",
		`map-blob # comment`:  "map-blob",
		"map-blob\t# comment": "map-blob",
		`"map#blob"`:          "map#blob",
		`'map#blob'`:          "map#blob",
		// A '#' that no whitespace precedes is content, not a comment marker —
		// bucket names cannot contain one, but the rule is the file's, not this
		// key's, and it is the rule modeltest and gcpkmstest read the same file by.
		`map#blob`: "map#blob",
	} {
		if got := parseValue(in); got != want {
			t.Errorf("parseValue(%q) = %q, want %q", in, got, want)
		}
	}
}
