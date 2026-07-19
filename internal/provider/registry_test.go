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

	before, err := reg.Provider("m")
	if err != nil {
		t.Fatal(err)
	}

	// Mutating the caller's map after construction must not reach the
	// provider's config — neither one built before the mutation nor one
	// built after it. The second is what pins NewRegistry's own clone:
	// without it r.routes aliases the caller's map, and since providers are
	// built per turn, an edit made at any point after startup would reach
	// every later turn.
	headers["x-tenant"] = "mallory"
	after, err := reg.Provider("m")
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]provider.Provider{"before": before, "after": after} {
		if got := p.(*fakeProvider).cfg.Headers["x-tenant"]; got != "acme" {
			t.Errorf("caller mutation leaked into the provider built %s it: %q", name, got)
		}
	}
}

// otherProvider stands in for a factory the registry was NOT built with.
type otherProvider struct{}

func (*otherProvider) Generate(context.Context, provider.Request) (provider.Stream, error) {
	return nil, nil
}

// TestRegistryOwnsTheFactoryTable pins the copy the registry makes of the
// factory table, for the same reason it copies each route's headers: the
// lock-free Provider path reads that table, so a caller left holding the
// original could redirect dispatch long after NewRegistry validated the routes
// against it. It substitutes rather than deletes, so the aliasing regression
// surfaces as this test's own message — deleting the entry would leave a nil
// Factory and report itself as a panic instead.
func TestRegistryOwnsTheFactoryTable(t *testing.T) {
	callers := map[string]provider.Factory{"anthropic": fakeFactory}
	reg, err := provider.NewRegistry([]provider.Route{
		{Model: "m", Config: provider.Config{Protocol: "anthropic", BaseURL: "http://gw"}},
	}, callers)
	if err != nil {
		t.Fatal(err)
	}

	callers["anthropic"] = func(provider.Config) (provider.Provider, error) {
		return &otherProvider{}, nil
	}
	p, err := reg.Provider("m")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*fakeProvider); !ok {
		t.Errorf("dispatch produced %T: the registry followed the caller's map", p)
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
