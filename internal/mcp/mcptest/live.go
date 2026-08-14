package mcptest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// LiveEnv opts into the live MCP tier: one real handshake and one real listing
// against a server this repository does not run. Any non-empty value opts in.
//
// It exists because every other MCP test in this repository speaks to a fixture
// built from the same SDK the client uses, so both ends share an understanding
// of the protocol and agree even where that understanding is wrong. A third
// party's server is the only thing that can find that out.
const LiveEnv = "RUN_LIVE_MCP_TESTS"

// The live server's coordinates, read from the environment or the repo-root
// .env. Exactly these two — nothing else ever reaches the file.
const (
	ServerURLEnv   = "MCP_LIVE_SERVER_URL"
	ServerTokenEnv = "MCP_LIVE_SERVER_TOKEN"
)

// LiveServer gates a live test and returns the server's endpoint and bearer
// token. Not opted in: the test skips and the credential file is never opened.
// Opted in with no endpoint: the test FAILS, because a live tier that skips
// itself when its configuration rots is not a live tier.
//
// The token is optional, and empty means an anonymous dial — which the protocol
// admits and the platform implements, so requiring one would fence off the
// public servers this tier is most likely to be pointed at. Nothing is lost by
// it: a server that wants a credential answers 401 when none arrives, and the
// test fails on that exactly as it would on a rotted one.
func LiveServer(t *testing.T) (url, token string) {
	t.Helper()
	url, token, skip, err := liveServer(resolve)
	if skip != "" {
		t.Skip(skip)
	}
	if err != nil {
		t.Fatal(err)
	}
	return url, token
}

// liveServer is the rule LiveServer applies, split out for its own tests.
func liveServer(getenv func(string) string) (url, token, skip string, err error) {
	if getenv(LiveEnv) == "" {
		return "", "", fmt.Sprintf(
			"%s is not set: skipping the live MCP tier (no third-party server is dialled)",
			LiveEnv), nil
	}
	url = getenv(ServerURLEnv)
	if url == "" {
		return "", "", "", fmt.Errorf("%s opted into the live MCP tier but %s is unset: "+
			"set it in the environment or the repo-root .env", LiveEnv, ServerURLEnv)
	}
	return url, getenv(ServerTokenEnv), "", nil
}

// resolve reads one key for the production gate.
func resolve(key string) string { return lookup(os.LookupEnv, dotEnv, key) }

// lookup resolves a key from the environment, falling back to the repo-root
// .env for the two server keys only. The environment always wins — including
// when it sets a key to the empty string, which is an answer ("this is unset")
// and not an invitation for the file to supply one. The tier variable lives
// outside those keys, so consent can never come from disk.
func lookup(lookupEnv func(string) (string, bool), file func() map[string]string, key string) string {
	if v, ok := lookupEnv(key); ok {
		return v
	}
	if key != ServerURLEnv && key != ServerTokenEnv {
		return ""
	}
	return file()[key]
}

// dotEnv parses the repo-root .env once, on first use. The values stay here
// rather than being pushed into the process environment: an os.Setenv would
// outlive the test that triggered it (modeltest's rationale, mirrored). A
// missing file is not an error — the environment may carry everything.
var dotEnv = sync.OnceValue(func() map[string]string {
	f, err := os.Open(filepath.Join(repoRoot(), ".env"))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	return parseDotEnv(f)
})

// repoRoot derives the checkout root from this file's compile-time path, so a
// caller reaches the same .env whatever directory it runs from.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func parseDotEnv(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		key = strings.TrimSpace(key)
		if key != ServerURLEnv && key != ServerTokenEnv {
			continue
		}
		out[key] = parseValue(value)
	}
	return out
}

// parseValue takes the value side of one .env line, exactly as modeltest reads
// the same file: a quoted value is whatever the quotes enclose, so a '#' inside
// them is content and anything after the closing quote is not; an unquoted one
// runs to a '#' that follows whitespace, which is a trailing comment, and keeps
// a '#' that does not — some credentials contain one.
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
