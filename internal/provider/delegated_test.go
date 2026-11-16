package provider_test

import (
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

func TestDelegatedRouteUsesOnlyTheCurrentClaim(t *testing.T) {
	routes, err := provider.LoadRoutes(writeConfig(t, `[
	  {"model":"gateway","protocol":"anthropic","base_url":"http://gateway","delegated_inference":true},
	  {"model":"vendor","protocol":"openai","base_url":"https://vendor.example","api_key":"vendor-key"}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := provider.NewRegistry(routes, factories)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, _ := reg.Describe("gateway")
	for _, token := range []string{"itk_first", "itk_second"} {
		for model, want := range map[string]string{"gateway": token, "vendor": "vendor-key"} {
			p, err := reg.ProviderForClaim(model, token)
			if err != nil {
				t.Fatal(err)
			}
			if p.(*fakeProvider).cfg.APIKey != want {
				t.Errorf("%s: wrong credential", model)
			}
		}
		if got, _ := reg.Describe("gateway"); got != descriptor {
			t.Error("claim changed the route's thinking identity")
		}
	}
	if _, err := reg.Provider("gateway"); err == nil {
		t.Error("delegated route accepted a call without a claim")
	}
	if _, err := reg.ProviderForClaim("gateway", ""); err == nil {
		t.Error("delegated route accepted an empty claim")
	}
	if _, err := reg.ProviderForClaim("missing", "itk_private"); err == nil || strings.Contains(err.Error(), "itk_private") {
		t.Error("missing route must fail without exposing the token")
	}
}

func TestDelegatedRouteRefusesStaticCredentials(t *testing.T) {
	t.Setenv("DELEGATED_TEST_KEY", "static")
	for _, fields := range []string{
		`"api_key":"static"`,
		`"api_key_env":"DELEGATED_TEST_KEY"`,
	} {
		if _, err := provider.LoadRoutes(writeConfig(t, `[{"model":"*","protocol":"anthropic","base_url":"http://gateway","delegated_inference":true,`+fields+`}]`)); err == nil || !strings.Contains(err.Error(), "delegated_inference excludes api_key and api_key_env") {
			t.Errorf("competing credential %s returned the wrong error: %v", fields, err)
		}
	}
	for _, cfg := range []provider.Config{
		{APIKey: "static"},
		{Headers: map[string]string{"Authorization": "Bearer static"}},
		{Headers: map[string]string{"X-Api-Key": "static"}},
	} {
		cfg.Protocol, cfg.BaseURL, cfg.DelegatedInference = "anthropic", "http://gateway", true
		if _, err := provider.NewRegistry([]provider.Route{{Model: "*", Config: cfg}}, factories); err == nil {
			t.Error("accepted a delegated route with static auth")
		}
	}
}
