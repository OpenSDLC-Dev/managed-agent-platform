package admin_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestDeploymentPriceMetadata(t *testing.T) {
	e := newEnv(t, false)
	const endpoint = "/admin/v1/deployments"
	const note = "Standard tier for input up to 512K tokens."
	pid := e.provider("deepseek", deepseekBoth)
	payload := map[string]any{"provider_id": pid, "upstream_model": "flash", "kind": "chat",
		"prices": map[string]any{"input": 2.1, "cache_read": 0}}
	check := func(got map[string]any, currency, terms string) {
		t.Helper()
		prices := got["prices"].(map[string]any)
		if got["price_currency"] != currency || got["price_note"] != terms ||
			prices["input"] != 2.1 || prices["output"] != nil || prices["cache_read"] != 0.0 {
			t.Fatalf("deployment prices = %v, currency = %q, note = %q", prices, got["price_currency"], got["price_note"])
		}
	}
	legacy := e.ok(e.admin("POST", endpoint, payload), "create with unspecified currency")
	check(legacy, "", "")
	payload["price_currency"], payload["price_note"] = "CNY", note
	created := e.ok(e.admin("POST", endpoint, payload), "create priced")
	check(created, "CNY", note)
	path := endpoint + "/" + created["id"].(string)
	check(e.ok(e.admin("GET", path, nil), "get"), "CNY", note)
	found := false
	for _, item := range e.admin("GET", endpoint, nil).list() {
		if item["id"] == created["id"] {
			check(item, "CNY", note)
			found = true
		}
	}
	if !found {
		t.Fatal("priced deployment missing from list")
	}
	check(e.ok(e.admin("POST", path, map[string]any{"enabled": false}), "legacy update"), "CNY", note)
	check(e.ok(e.admin("POST", path, map[string]any{"price_note": "高峰时段"}), "change terms"), "CNY", "高峰时段")
	check(e.ok(e.admin("POST", path, map[string]any{"price_currency": "USD"}), "change denomination"), "USD", "高峰时段")
	check(e.ok(e.admin("POST", path, map[string]any{"price_currency": "", "price_note": ""}), "clear"), "", "")
	for _, tc := range []struct{ field, value string }{
		{"price_currency", "cny"}, {"price_currency", "CN"}, {"price_currency", "CN1"},
		{"price_currency", "ＣＮＹ"}, {"price_currency", "CNY\n"},
		{"price_note", strings.Repeat("价", 257)}, {"price_note", "standard\npriority"},
	} {
		body := map[string]any{"provider_id": pid, "upstream_model": "flash", "kind": "chat", tc.field: tc.value}
		e.refused(e.admin("POST", endpoint, body), http.StatusBadRequest, "invalid_request_error", tc.field)
		e.refused(e.admin("POST", path, map[string]any{tc.field: tc.value}), http.StatusBadRequest, "invalid_request_error", tc.field)
	}
	for _, field := range []string{"price_currency", "price_note"} {
		e.refused(e.admin("POST", path, map[string]any{field: nil}), http.StatusBadRequest, "invalid_request_error", "null")
	}
	check(e.ok(e.admin("GET", path, nil), "after rejected changes"), "", "")
}
