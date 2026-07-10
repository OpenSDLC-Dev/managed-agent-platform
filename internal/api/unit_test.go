package api

import (
	"encoding/base64"
	"net/url"
	"testing"
	"time"
)

func TestCursorRoundTrip(t *testing.T) {
	for _, off := range []int{0, 1, 20, 12345} {
		got, err := decodeCursor(encodeCursor(off))
		if err != nil || got != off {
			t.Errorf("round-trip %d: got %d, err %v", off, got, err)
		}
	}
	for name, cursor := range map[string]string{
		"not base64":      "@@@",
		"wrong prefix":    base64.RawURLEncoding.EncodeToString([]byte("x9.5")),
		"negative offset": base64.RawURLEncoding.EncodeToString([]byte("o1.-3")),
		"non-numeric":     base64.RawURLEncoding.EncodeToString([]byte("o1.abc")),
	} {
		if _, err := decodeCursor(cursor); err == nil {
			t.Errorf("%s: decodeCursor accepted %q", name, cursor)
		}
	}
}

func TestPrevCursorClampsToZero(t *testing.T) {
	p := pageParams{limit: 20, offset: 5}
	prev := p.prevCursor()
	if prev == nil {
		t.Fatal("prevCursor = nil for offset 5")
	}
	if off, err := decodeCursor(*prev); err != nil || off != 0 {
		t.Errorf("prev offset = %d (%v), want 0", off, err)
	}
	if (pageParams{limit: 20}).prevCursor() != nil {
		t.Error("prevCursor at offset 0 should be nil")
	}
}

func TestParsePageRejectsBadLimits(t *testing.T) {
	for _, q := range []string{"limit=abc", "limit=-1", "limit=0", "limit=101"} {
		vals, _ := url.ParseQuery(q)
		if _, err := parsePage(vals); err == nil {
			t.Errorf("parsePage accepted %q", q)
		}
	}
	vals, _ := url.ParseQuery("")
	p, err := parsePage(vals)
	if err != nil || p.limit != defaultLimit || p.offset != 0 {
		t.Errorf("defaults = %+v (%v)", p, err)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	e := errInvalid("bad %s", "field")
	if e.Error() != "bad field" {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestRenderAgentDefaultsNilCollections(t *testing.T) {
	out := renderAgent("agent_x", "n", 1, agentSpec{}, nil, time.Time{}, time.Time{}, nil)
	if out.Metadata == nil || out.Tools == nil || out.MCPServers == nil || out.Skills == nil {
		t.Errorf("renderAgent left nil collections: %+v", out)
	}
}

func TestRenderEnvironmentDefaultsNilMetadata(t *testing.T) {
	out := renderEnvironment("env_x", "n", "", nil, nil, time.Time{}, time.Time{}, nil)
	if out.Metadata == nil {
		t.Error("renderEnvironment left nil metadata")
	}
}

// TestRenderSessionDefaultsNilCollections covers stored rows that predate a
// field (jsonb null / missing keys): rendering must still emit the full
// required wire surface.
func TestRenderSessionDefaultsNilCollections(t *testing.T) {
	out, err := renderSession(sessionRow{
		id: "sesn_x", agentJSON: []byte(`{}`), metaJSON: []byte(`{}`),
		usageJSON: []byte(`{}`), resourcesJSON: []byte(`null`),
	})
	if err != nil {
		t.Fatalf("renderSession: %v", err)
	}
	if out.Agent.Tools == nil || out.Agent.MCPServers == nil || out.Agent.Skills == nil {
		t.Errorf("agent collections nil: %+v", out.Agent)
	}
	if out.Resources == nil || out.VaultIDs == nil || out.OutcomeEvaluations == nil {
		t.Errorf("session collections nil: %+v", out)
	}
}
