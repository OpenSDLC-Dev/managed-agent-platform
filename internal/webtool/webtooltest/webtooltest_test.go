package webtooltest

import (
	"strings"
	"testing"
)

func mapGetenv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLiveKeyGate(t *testing.T) {
	t.Run("not opted in skips without reading a key", func(t *testing.T) {
		_, skip, err := liveKey(mapGetenv(map[string]string{TavilyKeyEnv: "tvly-x"}), TavilyKeyEnv)
		if err != nil {
			t.Fatalf("liveKey: %v", err)
		}
		if !strings.Contains(skip, LiveEnv) {
			t.Errorf("skip message %q does not name %s", skip, LiveEnv)
		}
	})

	t.Run("opted in with the key missing fails naming it", func(t *testing.T) {
		_, skip, err := liveKey(mapGetenv(map[string]string{LiveEnv: "1"}), JinaKeyEnv)
		if skip != "" {
			t.Fatalf("skipped: %q", skip)
		}
		if err == nil || !strings.Contains(err.Error(), JinaKeyEnv) {
			t.Errorf("err = %v, want one naming %s", err, JinaKeyEnv)
		}
	})

	t.Run("opted in and configured returns the key", func(t *testing.T) {
		key, skip, err := liveKey(mapGetenv(map[string]string{LiveEnv: "1", TavilyKeyEnv: "tvly-x"}), TavilyKeyEnv)
		if skip != "" || err != nil {
			t.Fatalf("skip=%q err=%v", skip, err)
		}
		if key != "tvly-x" {
			t.Errorf("key = %q, want %q", key, "tvly-x")
		}
	})

	t.Run("any non-empty value opts in", func(t *testing.T) {
		_, skip, _ := liveKey(mapGetenv(map[string]string{LiveEnv: "0", TavilyKeyEnv: "k"}), TavilyKeyEnv)
		if skip != "" {
			t.Errorf("RUN_LIVE_WEB_TESTS=0 read as not-opted-in: consent is naming the variable, not its value")
		}
	})
}

func TestLookup(t *testing.T) {
	file := func() map[string]string {
		return map[string]string{TavilyKeyEnv: "from-file", JinaKeyEnv: "from-file"}
	}

	t.Run("the environment wins", func(t *testing.T) {
		env := func(k string) (string, bool) { return "from-env", true }
		if got := lookup(env, file, TavilyKeyEnv); got != "from-env" {
			t.Errorf("lookup = %q, want from-env", got)
		}
	})

	t.Run("an empty environment value stands", func(t *testing.T) {
		env := func(k string) (string, bool) { return "", true }
		if got := lookup(env, file, TavilyKeyEnv); got != "" {
			t.Errorf("lookup = %q, want empty: an explicit unset is an answer", got)
		}
	})

	t.Run("a web key falls back to the file", func(t *testing.T) {
		env := func(k string) (string, bool) { return "", false }
		if got := lookup(env, file, JinaKeyEnv); got != "from-file" {
			t.Errorf("lookup = %q, want from-file", got)
		}
	})

	t.Run("a non-web key never reaches the file", func(t *testing.T) {
		env := func(k string) (string, bool) { return "", false }
		poisoned := func() map[string]string {
			t.Error("the file was opened for a non-web key")
			return nil
		}
		if got := lookup(env, poisoned, LiveEnv); got != "" {
			t.Errorf("lookup = %q, want empty", got)
		}
	})
}

func TestParseDotEnv(t *testing.T) {
	got := parseDotEnv(strings.NewReader(strings.Join([]string{
		"# a comment",
		"",
		"MODEL_API_KEY=not-ours",
		"TAVILY_API_KEY=tvly-abc # trailing comment",
		`JINA_API_KEY="jina with # inside" ignored-tail`,
		"RUN_LIVE_WEB_TESTS=1",
	}, "\n")))
	want := map[string]string{
		TavilyKeyEnv: "tvly-abc",
		JinaKeyEnv:   "jina with # inside",
	}
	if len(got) != len(want) {
		t.Errorf("parseDotEnv kept %d keys, want %d (%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["RUN_LIVE_WEB_TESTS"]; ok {
		t.Error("the tier variable was read from disk: consent must come from the environment")
	}
}
