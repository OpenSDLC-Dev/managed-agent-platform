package tavily_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool/tavily"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool/webtooltest"
)

// TestLiveSearch is the live tier: one real Tavily search. Consent and
// credentials per webtooltest.LiveKey.
func TestLiveSearch(t *testing.T) {
	key := webtooltest.LiveKey(t, webtooltest.TavilyKeyEnv)
	hits, err := tavily.New("", key).Search(context.Background(), "Go programming language")
	if err != nil {
		t.Fatalf("live search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("live search returned no hits")
	}
	for i, h := range hits {
		if !strings.HasPrefix(h.URL, "http") {
			t.Errorf("hit %d URL = %q, not http(s)", i, h.URL)
		}
	}
}
