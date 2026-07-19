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

func TestRegistryIsolatesCallerConfig(t *testing.T) {
	headers := map[string]string{"x-tenant": "acme"}
	routes := []provider.Route{{Model: "m", Config: provider.Config{
		Protocol: "anthropic", BaseURL: "http://gw", Headers: headers}}}
	reg, err := provider.NewRegistry(routes, factories)
	if err != nil {
		t.Fatal(err)
	}

	p, err := reg.Provider("m")
	if err != nil {
		t.Fatal(err)
	}

	// Mutating the caller's map after construction must not reach the
	// provider's config.
	headers["x-tenant"] = "mallory"
	if got := p.(*fakeProvider).cfg.Headers["x-tenant"]; got != "acme" {
		t.Errorf("caller mutation leaked into provider config: %q", got)
	}
}

// TestRegistryOwnsTheFactoryTable pins the copy the registry makes of the
// factory table, for the same reason it copies each route's headers: the
// lock-free Provider path reads that table, so a caller left holding the
// original could redirect dispatch — or, by deleting the entry, panic it —
// long after NewRegistry validated the routes against it.
func TestRegistryOwnsTheFactoryTable(t *testing.T) {
	callers := map[string]provider.Factory{"anthropic": fakeFactory}
	reg, err := provider.NewRegistry([]provider.Route{
		{Model: "m", Config: provider.Config{Protocol: "anthropic", BaseURL: "http://gw"}},
	}, callers)
	if err != nil {
		t.Fatal(err)
	}

	delete(callers, "anthropic")
	p, err := reg.Provider("m")
	if err != nil {
		t.Fatalf("dispatch followed the caller's map: %v", err)
	}
	if _, ok := p.(*fakeProvider); !ok {
		t.Errorf("dispatch produced %T, not the factory the registry was built with", p)
	}
}

// TestRegistryRetainsNothingPerModelString fences issue #88's memory half. The
// registry used to cache constructed providers under the agent's model string,
// which a "*" route makes client-controlled: every distinct string a client
// invented retained an entry for the life of the brain process. Nothing is
// cached now, and a returned instance can only be a cache hit if it is the same
// instance — so non-identity is the observable that a reintroduced
// string-keyed cache would break.
func TestRegistryRetainsNothingPerModelString(t *testing.T) {
	reg, err := provider.NewRegistry([]provider.Route{
		{Model: "*", Config: provider.Config{Protocol: "anthropic", BaseURL: "http://gw"}},
	}, factories)
	if err != nil {
		t.Fatal(err)
	}

	p1, err := reg.Provider("invented-by-a-client")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := reg.Provider("invented-by-a-client")
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Error("one model string was served the same instance twice: the registry is retaining providers keyed by client input")
	}

	// Each pass-through provider still carries the string it was built for.
	for _, model := range []string{"one", "two", "three"} {
		p, err := reg.Provider(model)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.(*fakeProvider).cfg.Model; got != model {
			t.Errorf("pass-through provider for %q got model %q", model, got)
		}
	}
}

// TestRegistryDefaultRouteWithUpstreamModelIgnoresClientString covers the half
// of the leak issue #88 does not describe: the old cache stored under the
// client's string whichever branch the route took, so a "*" route that DOES
// name an upstream model retained one byte-identical provider per distinct
// string too. A fix that only skipped the cache for the pass-through would
// have left this in place.
func TestRegistryDefaultRouteWithUpstreamModelIgnoresClientString(t *testing.T) {
	reg, err := provider.NewRegistry([]provider.Route{
		{Model: "*", Config: provider.Config{
			Protocol: "anthropic", BaseURL: "http://gw", Model: "upstream-fixed"}},
	}, factories)
	if err != nil {
		t.Fatal(err)
	}

	for _, model := range []string{"a", "b", "c"} {
		first, err := reg.Provider(model)
		if err != nil {
			t.Fatal(err)
		}
		if got := first.(*fakeProvider).cfg.Model; got != "upstream-fixed" {
			t.Errorf("client string %q reached the endpoint as model %q", model, got)
		}
		second, err := reg.Provider(model)
		if err != nil {
			t.Fatal(err)
		}
		if first == second {
			t.Errorf("model %q was served a retained instance: the registry still keys on client input", model)
		}
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
		{"duplicate default route", []provider.Route{
			{Model: "*", Config: provider.Config{Protocol: "anthropic", BaseURL: "http://x"}},
			{Model: "*", Config: provider.Config{Protocol: "anthropic", BaseURL: "http://y"}},
		}},
	}
	for _, tc := range cases {
		if _, err := provider.NewRegistry(tc.routes, factories); err == nil {
			t.Errorf("%s: NewRegistry accepted an invalid route set", tc.name)
		}
	}
}
