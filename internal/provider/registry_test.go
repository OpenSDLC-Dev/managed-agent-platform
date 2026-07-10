package provider_test

import (
	"context"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
)

// fakeProvider records the config it was built from.
type fakeProvider struct{ cfg provider.Config }

func (f *fakeProvider) Generate(context.Context, provider.Request) (provider.Stream, error) {
	return nil, nil
}

func fakeFactory(cfg provider.Config) (provider.Provider, error) {
	return &fakeProvider{cfg: cfg}, nil
}

var factories = map[string]provider.Factory{"anthropic": fakeFactory}

func TestRegistryRouting(t *testing.T) {
	reg, err := provider.NewRegistry([]provider.Route{
		{Model: "claude-opus-4-8", Config: provider.Config{
			Protocol: "anthropic", BaseURL: "http://gw-a", Model: "upstream-opus"}},
		{Model: "*", Config: provider.Config{
			Protocol: "anthropic", BaseURL: "http://gw-default"}},
	}, factories)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Exact route: the configured upstream model wins.
	p, err := reg.Provider("claude-opus-4-8")
	if err != nil {
		t.Fatal(err)
	}
	got := p.(*fakeProvider).cfg
	if got.BaseURL != "http://gw-a" || got.Model != "upstream-opus" {
		t.Errorf("exact route config = %+v", got)
	}

	// Fallback route with no upstream model: the agent's model string
	// passes through to the endpoint.
	p, err = reg.Provider("some-internal-llm")
	if err != nil {
		t.Fatal(err)
	}
	got = p.(*fakeProvider).cfg
	if got.BaseURL != "http://gw-default" || got.Model != "some-internal-llm" {
		t.Errorf("fallback config = %+v", got)
	}
}

func TestRegistryNoFallback(t *testing.T) {
	reg, err := provider.NewRegistry([]provider.Route{
		{Model: "known", Config: provider.Config{Protocol: "anthropic", BaseURL: "http://gw"}},
	}, factories)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Provider("unknown"); err == nil {
		t.Error("unrouted model without a default should error")
	}
}

func TestRegistryValidation(t *testing.T) {
	cases := []struct {
		name   string
		routes []provider.Route
	}{
		{"empty model", []provider.Route{{Model: "", Config: provider.Config{Protocol: "anthropic", BaseURL: "http://x"}}}},
		{"missing base_url", []provider.Route{{Model: "m", Config: provider.Config{Protocol: "anthropic"}}}},
		{"missing protocol", []provider.Route{{Model: "m", Config: provider.Config{BaseURL: "http://x"}}}},
		{"unknown protocol", []provider.Route{{Model: "m", Config: provider.Config{Protocol: "carrier-pigeon", BaseURL: "http://x"}}}},
		{"duplicate route", []provider.Route{
			{Model: "m", Config: provider.Config{Protocol: "anthropic", BaseURL: "http://x"}},
			{Model: "m", Config: provider.Config{Protocol: "anthropic", BaseURL: "http://y"}},
		}},
	}
	for _, tc := range cases {
		if _, err := provider.NewRegistry(tc.routes, factories); err == nil {
			t.Errorf("%s: NewRegistry accepted an invalid route set", tc.name)
		}
	}
}
