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
	// This is the only test that sees Tavily's real JSON, so it must pin the
	// field mapping the stub suites mirror onto themselves: a hit with an
	// empty Title or Content here means the adapter is decoding the wrong
	// field names, whatever the stubs say.
	var titled, contentful bool
	for i, h := range hits {
		if !strings.HasPrefix(h.URL, "http") {
			t.Errorf("hit %d URL = %q, not http(s)", i, h.URL)
		}
		titled = titled || h.Title != ""
		contentful = contentful || h.Content != ""
	}
	if !titled {
		t.Error("no hit carried a title: the title field mapping is wrong")
	}
	if !contentful {
		t.Error("no hit carried content: the content field mapping is wrong")
	}
}
