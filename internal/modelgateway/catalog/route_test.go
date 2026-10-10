package catalog

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

var both = []profile.Protocol{profile.Anthropic, profile.OpenAI}

// routing is a configuration of two providers: p1 serves both protocols
// with credentials c1a (weight 3) and c1b (weight 1), p2 Anthropic only with
// c2. Deployments d1 and d2 are on p1, d3 on p2.
func routing() store.Config {
	return store.Config{
		Providers: []store.Provider{
			{ID: "p1", Enabled: true, Endpoints: map[profile.Protocol]string{profile.Anthropic: "https://one/anthropic", profile.OpenAI: "https://one/v1"}},
			{ID: "p2", Enabled: true, Endpoints: map[profile.Protocol]string{profile.Anthropic: "https://two/anthropic"}},
		},
		Credentials: []store.Credential{
			{ID: "c1a", ProviderID: "p1", Protocols: both, Weight: 3, Enabled: true},
			{ID: "c1b", ProviderID: "p1", Protocols: both, Weight: 1, Enabled: true},
			{ID: "c2", ProviderID: "p2", Protocols: []profile.Protocol{profile.Anthropic}, Weight: 1, Enabled: true},
		},
		Deployments: []store.Deployment{
			{ID: "d1", ProviderID: "p1", Kind: store.KindChat, Enabled: true},
			{ID: "d2", ProviderID: "p1", Kind: store.KindChat, Enabled: true},
			{ID: "d3", ProviderID: "p2", Kind: store.KindChat, Enabled: true},
		},
	}
}

func ids(as []Attempt) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Deployment.ID + "/" + a.Credential.ID
	}
	return out
}

// fixed returns a draw that yields us in turn.
func fixed(us ...float64) func() float64 {
	i := 0
	return func() float64 { u := us[i%len(us)]; i++; return u }
}

func TestPlanOrdersGroupsThenDeploymentsThenCredentials(t *testing.T) {
	s := newSnapshot(routing())
	a := store.Alias{Name: "m", Targets: []store.Target{
		{DeploymentID: "d3", Priority: 1, Weight: 1},
		{DeploymentID: "d1", Priority: 0, Weight: 1},
	}}
	// Draws go to the group's deployments, then to each deployment's
	// credentials: d1's c1a and c1b draw 0.5 each, so weight decides.
	got := ids(s.Plan(a, profile.Anthropic, "", fixed(0.5)))
	want := []string{"d1/c1a", "d1/c1b", "d3/c2"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("plan = %v, want %v", got, want)
	}
	if e := s.Plan(a, profile.Anthropic, "", fixed(0.5))[2].Endpoint; e != "https://two/anthropic" {
		t.Errorf("endpoint = %q", e)
	}
}

func TestPlanSkipsWhatIsNotEligible(t *testing.T) {
	cases := map[string]func(*store.Config){
		"deployment disabled": func(c *store.Config) { c.Deployments[2].Enabled = false },
		"provider disabled":   func(c *store.Config) { c.Providers[1].Enabled = false },
		"credential disabled": func(c *store.Config) { c.Credentials[2].Enabled = false },
		"no endpoint":         func(c *store.Config) { delete(c.Providers[1].Endpoints, profile.Anthropic) },
		"credential not on the protocol": func(c *store.Config) {
			c.Credentials[2].Protocols = []profile.Protocol{profile.OpenAI}
		},
		"deployment gone": func(c *store.Config) { c.Deployments = c.Deployments[:2] },
	}
	a := store.Alias{Name: "m", Targets: []store.Target{{DeploymentID: "d1", Weight: 1}, {DeploymentID: "d3", Priority: 1, Weight: 1}}}
	for name, break_ := range cases {
		cfg := routing()
		break_(&cfg)
		got := ids(newSnapshot(cfg).Plan(a, profile.Anthropic, "", fixed(0.5)))
		if fmt.Sprint(got) != fmt.Sprint([]string{"d1/c1a", "d1/c1b"}) {
			t.Errorf("%s: plan = %v, want d3 left out", name, got)
		}
	}
	// On OpenAI only p1 has an endpoint.
	if got := ids(newSnapshot(routing()).Plan(a, profile.OpenAI, "", fixed(0.5))); len(got) != 2 {
		t.Errorf("openai plan = %v", got)
	}
}

// A Messages request reaches each credential on the protocol it is allowed
// on: its own where it can, else OpenAI's, which the gateway converts to. A
// Chat Completions request is never converted.
func TestPlanConvertsOnlyWhereItMust(t *testing.T) {
	cfg := routing()
	cfg.Credentials[1].Protocols = []profile.Protocol{profile.OpenAI}
	a := store.Alias{Name: "m", Targets: []store.Target{{DeploymentID: "d1", Weight: 1}}}
	var got []string
	for _, at := range newSnapshot(cfg).Plan(a, profile.Anthropic, "", fixed(0.5)) {
		got = append(got, at.Credential.ID+" "+string(at.Protocol)+" "+at.Endpoint)
	}
	want := []string{"c1a anthropic https://one/anthropic", "c1b openai https://one/v1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("plan = %v, want %v", got, want)
	}
	cfg = routing()
	cfg.Credentials[2].Protocols = []profile.Protocol{profile.OpenAI}
	cfg.Providers[0].Endpoints = map[profile.Protocol]string{profile.Anthropic: "https://one/anthropic"}
	a = store.Alias{Name: "m", Targets: []store.Target{{DeploymentID: "d1", Weight: 1}, {DeploymentID: "d3", Weight: 1}}}
	if got := newSnapshot(cfg).Plan(a, profile.OpenAI, "", fixed(0.5)); len(got) != 0 {
		t.Errorf("a Chat Completions request reached %v", ids(got))
	}
}

// A Messages request reaches a Gemini provider on the Gemini protocol, which
// the gateway converts to, after its own and OpenAI's; no other request does.
func TestPlanConvertsToGemini(t *testing.T) {
	cfg := routing()
	cfg.Providers = append(cfg.Providers, store.Provider{ID: "p3", Enabled: true,
		Endpoints: map[profile.Protocol]string{profile.Gemini: "https://g/v1beta"}})
	cfg.Credentials = append(cfg.Credentials, store.Credential{ID: "c3", ProviderID: "p3", Protocols: []profile.Protocol{profile.Gemini}, Weight: 1, Enabled: true})
	cfg.Deployments = append(cfg.Deployments, store.Deployment{ID: "d4", ProviderID: "p3", Kind: store.KindChat, Enabled: true})
	a := store.Alias{Name: "m", Targets: []store.Target{{DeploymentID: "d4", Weight: 1}}}
	s := newSnapshot(cfg)
	got := s.Plan(a, profile.Anthropic, "", fixed(0.5))
	if len(got) != 1 || got[0].Protocol != profile.Gemini || got[0].Endpoint != "https://g/v1beta" {
		t.Errorf("plan = %+v", got)
	}
	if got := s.Plan(a, profile.OpenAI, "", fixed(0.5)); len(got) != 0 {
		t.Errorf("a Chat Completions request reached %v", ids(got))
	}
	cfg.Providers[2].Endpoints[profile.OpenAI] = "https://g/openai"
	cfg.Credentials[3].Protocols = []profile.Protocol{profile.Gemini, profile.OpenAI}
	if got := newSnapshot(cfg).Plan(a, profile.Anthropic, "", fixed(0.5)); len(got) != 1 || got[0].Protocol != profile.OpenAI {
		t.Errorf("plan = %+v, want the OpenAI conversion first", got)
	}
}

// Over many draws, a deployment comes first in proportion to its weight.
func TestPlanDrawsByWeight(t *testing.T) {
	s := newSnapshot(routing())
	a := store.Alias{Name: "m", Targets: []store.Target{{DeploymentID: "d1", Weight: 3}, {DeploymentID: "d3", Weight: 1}}}
	rng := rand.New(rand.NewPCG(1, 2))
	draw := func() float64 { return 1 - rng.Float64() }
	first := 0
	const n = 20000
	for range n {
		if s.Plan(a, profile.Anthropic, "", draw)[0].Deployment.ID == "d1" {
			first++
		}
	}
	if share := float64(first) / n; share < 0.72 || share > 0.78 {
		t.Errorf("d1 first in %.3f of plans, want about 0.75", share)
	}
}

// A session keeps its deployment and credential whatever the draw, and the
// sessions between them still divide by weight.
func TestPlanSticksToASession(t *testing.T) {
	s := newSnapshot(routing())
	a := store.Alias{Name: "m", Targets: []store.Target{{DeploymentID: "d1", Weight: 3}, {DeploymentID: "d2", Weight: 1}}}
	one := ids(s.Plan(a, profile.Anthropic, "sesn_a", fixed(0.01)))
	two := ids(s.Plan(a, profile.Anthropic, "sesn_a", fixed(0.99)))
	if fmt.Sprint(one) != fmt.Sprint(two) {
		t.Errorf("one session, two plans: %v and %v", one, two)
	}
	first := 0
	const n = 4000
	for i := range n {
		if s.Plan(a, profile.Anthropic, fmt.Sprintf("sesn_%d", i), nil)[0].Deployment.ID == "d1" {
			first++
		}
	}
	if share := float64(first) / n; share < 0.70 || share > 0.80 {
		t.Errorf("d1 first for %.3f of sessions, want about 0.75", share)
	}
}

func TestAliasesAreOrderedByName(t *testing.T) {
	s := newSnapshot(store.Config{Aliases: []store.Alias{{Name: "b"}, {Name: "a"}, {Name: "c"}}})
	var got []string
	for _, a := range s.Aliases() {
		got = append(got, a.Name)
	}
	if fmt.Sprint(got) != "[a b c]" {
		t.Errorf("aliases = %v", got)
	}
}
