package jina_test

import (
	"context"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool/jina"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/webtool/webtooltest"
)

// TestLiveFetch is the live tier: one real Jina Reader fetch of a stable
// page. Consent and credentials per webtooltest.LiveKey.
func TestLiveFetch(t *testing.T) {
	key := webtooltest.LiveKey(t, webtooltest.JinaKeyEnv)
	got, err := jina.New("", key).Fetch(context.Background(), "https://example.com/")
	if err != nil {
		t.Fatalf("live fetch: %v", err)
	}
	if !strings.Contains(got.Content, "Example Domain") {
		t.Errorf("fetched content does not contain %q (len %d)", "Example Domain", len(got.Content))
	}
	if got.Truncated {
		t.Error("example.com reported truncated")
	}
}
