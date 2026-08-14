package mcptest

import (
	"strings"
	"testing"
)

// The gate's whole rule, on the pure function rather than through the process
// environment: consent comes from the tier variable and never from the file, an
// opted-in tier with no endpoint fails rather than skips, and an absent token is
// an anonymous dial rather than a misconfiguration.
func TestLiveServerGate(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	t.Run("not opted in skips and asks for nothing", func(t *testing.T) {
		// A poisoned getenv rather than a populated one: the promise is that the
		// credential file is never opened, and a resolver that read the keys and
		// discarded them would satisfy any assertion about the return values.
		poisoned := func(k string) string {
			if k != LiveEnv {
				t.Errorf("resolved %s before checking consent: the credential file "+
					"must not be opened on a run that is not opted in", k)
			}
			return ""
		}
		url, token, skip, err := liveServer(poisoned)
		if skip == "" || err != nil || url != "" || token != "" {
			t.Fatalf("liveServer = (%q, %q, %q, %v), want a skip and nothing else",
				url, token, skip, err)
		}
		if !strings.Contains(skip, LiveEnv) {
			t.Errorf("skip = %q, want it to name %s so a reader knows what to set", skip, LiveEnv)
		}
	})

	t.Run("consent is naming the variable, not its value", func(t *testing.T) {
		// RUN_LIVE_MCP_TESTS=0 opts in. Someone who sets it to 0 meaning "off"
		// gets a live run, which is surprising — but the alternative is parsing
		// the value, and then a typo means a silent skip. Loud and documented
		// beats quiet and clever, and it is what every sibling tier does.
		url, _, skip, err := liveServer(env(map[string]string{
			LiveEnv: "0", ServerURLEnv: "https://example.test/mcp",
		}))
		if skip != "" || err != nil {
			t.Fatalf("liveServer = (%q, %v), want an opted-in run", skip, err)
		}
		if url != "https://example.test/mcp" {
			t.Errorf("url = %q, want the configured endpoint", url)
		}
	})

	t.Run("opted in with no endpoint fails rather than skipping", func(t *testing.T) {
		_, _, skip, err := liveServer(env(map[string]string{LiveEnv: "1"}))
		if skip != "" {
			t.Fatalf("skip = %q, want a failure: a tier that skips when its "+
				"configuration rots is not a tier", skip)
		}
		if err == nil || !strings.Contains(err.Error(), ServerURLEnv) {
			t.Fatalf("err = %v, want it to name %s", err, ServerURLEnv)
		}
	})

	t.Run("opted in with no token dials anonymously", func(t *testing.T) {
		url, token, skip, err := liveServer(env(map[string]string{
			LiveEnv: "1", ServerURLEnv: "https://example.test/mcp",
		}))
		if skip != "" || err != nil {
			t.Fatalf("liveServer = (%q, %q, %v), want the endpoint and no token",
				skip, token, err)
		}
		if url != "https://example.test/mcp" || token != "" {
			t.Errorf("liveServer = (%q, %q), want the endpoint with an empty token", url, token)
		}
	})
}

// The environment wins over the file, and consent is never readable from it.
func TestLookupPrefersTheEnvironment(t *testing.T) {
	file := func() map[string]string {
		return map[string]string{
			ServerURLEnv: "https://file.test/mcp", ServerTokenEnv: "file-token", LiveEnv: "1",
		}
	}
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}

	if got := lookup(env(map[string]string{ServerURLEnv: "https://env.test/mcp"}), file,
		ServerURLEnv); got != "https://env.test/mcp" {
		t.Errorf("url = %q, want the environment's", got)
	}
	// Set to empty is an answer, not an invitation for the file to supply one.
	if got := lookup(env(map[string]string{ServerTokenEnv: ""}), file, ServerTokenEnv); got != "" {
		t.Errorf("token = %q, want the environment's empty value to stand", got)
	}
	if got := lookup(env(nil), file, ServerURLEnv); got != "https://file.test/mcp" {
		t.Errorf("url = %q, want the file's when the environment is silent", got)
	}
	// The tier variable is outside the two keys the file may answer for, so a
	// .env can never opt a run into spending anything.
	if got := lookup(env(nil), file, LiveEnv); got != "" {
		t.Errorf("%s = %q from the file: consent must come from the environment alone",
			LiveEnv, got)
	}
}

// Only the two server keys are ever taken from the file, and a value is read the
// way modeltest reads the same file.
func TestParseDotEnv(t *testing.T) {
	got := parseDotEnv(strings.NewReader(strings.Join([]string{
		"# a comment",
		ServerURLEnv + " = https://example.test/mcp   # trailing",
		ServerTokenEnv + `="tok#en"`,
		"MODEL_API_KEY=not-ours",
		LiveEnv + "=1",
		"malformed-line",
	}, "\n")))
	want := map[string]string{
		ServerURLEnv:   "https://example.test/mcp",
		ServerTokenEnv: "tok#en",
	}
	if len(got) != len(want) {
		t.Fatalf("parseDotEnv = %v, want only the two server keys (%v)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parseDotEnv[%s] = %q, want %q", k, got[k], v)
		}
	}
}
