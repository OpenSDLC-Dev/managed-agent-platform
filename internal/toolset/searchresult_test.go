package toolset_test

import (
	"encoding/json"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// The search_result block the executor's web driver emits (carried on
// Result.SearchResults) must be, field for field, what the SDK's typed schema
// decodes — the round-trip discipline every
// wire shape gets. Presence is asserted through respjson, not just decoded
// values: a dropped required field would decode to the same zero value.
func TestSearchResultBlockRoundTripsThroughSDK(t *testing.T) {
	raw, err := json.Marshal(domain.SearchResultBlock{
		Type:      "search_result",
		Citations: domain.SearchResultCitations{Enabled: false},
		Content:   []domain.ContentBlock{{Type: "text", Text: "How to write Go."}},
		Source:    "https://go.dev/doc/",
		Title:     "Go docs",
	})
	if err != nil {
		t.Fatal(err)
	}

	var got sdk.BetaManagedAgentsSearchResultBlock
	if err := got.UnmarshalJSON(raw); err != nil {
		t.Fatalf("SDK decode: %v", err)
	}
	if string(got.Type) != "search_result" || got.Title != "Go docs" || got.Source != "https://go.dev/doc/" {
		t.Errorf("decoded block = %+v", got)
	}
	if len(got.Content) != 1 || string(got.Content[0].Type) != "text" || got.Content[0].Text != "How to write Go." {
		t.Errorf("decoded content = %+v", got.Content)
	}
	if got.Citations.Enabled {
		t.Error("citations.enabled decoded true, want false")
	}

	for name, valid := range map[string]bool{
		"type":              got.JSON.Type.Valid(),
		"citations":         got.JSON.Citations.Valid(),
		"content":           got.JSON.Content.Valid(),
		"source":            got.JSON.Source.Valid(),
		"title":             got.JSON.Title.Valid(),
		"citations.enabled": got.Citations.JSON.Enabled.Valid(),
		"content[0].type":   got.Content[0].JSON.Type.Valid(),
		"content[0].text":   got.Content[0].JSON.Text.Valid(),
	} {
		if !valid {
			t.Errorf("wire-required field %s absent from the marshaled JSON", name)
		}
	}
}
