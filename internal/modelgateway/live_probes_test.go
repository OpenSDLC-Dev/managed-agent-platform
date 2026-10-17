package modelgateway_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// TestLiveProbes drives the admin API's credential check and deployment
// test (#953) at every named vendor, straight to its hosts in the region of
// the tier's key:
//   - a check lists models on each protocol the profile records a listing
//     for, the tier's chat models among them, and sends no request where it
//     records none; a bad key is refused on each listing protocol, its
//     status reported;
//   - a test of each chat model, on its Anthropic endpoint by passthrough
//     and on its OpenAI one through the conversion, asks for one token and
//     is answered 200 with the vendor's usage; so are Gitee's and Zhipu's
//     embeddings and Gitee's rerank. Zhipu's chat is tested on its OpenAI
//     endpoint alone, and Gemini's not at all, for the reasons liveVendors
//     gives;
//   - the ledger holds a test row for each test made.
func TestLiveProbes(t *testing.T) {
	vendors := namedVendors(t)
	e := newEnv(t)
	type checked struct {
		v         liveVendor
		p         store.Provider
		good, bad store.Credential
	}
	type tested struct {
		v     liveVendor
		d     store.Deployment
		proto profile.Protocol
	}
	var checks []checked
	var tests []tested
	on := func(v liveVendor, endpoints map[profile.Protocol]string) store.Provider {
		return e.provider("", func(p *store.Provider) { p.Name, p.Profile, p.Endpoints = v.name, v.name, endpoints })
	}
	for _, v := range vendors {
		hosts := liveHosts(t, v)
		p := on(v, hosts)
		checks = append(checks, checked{v: v, p: p, good: e.credential(p, v.keyEnv, 1), bad: e.credential(p, "sk-bad-key-0000000000000000", 1)})
		if v.name == "gemini" {
			continue
		}
		for _, proto := range []profile.Protocol{profile.Anthropic, profile.OpenAI} {
			if hosts[proto] == "" || proto == profile.Anthropic && v.openAIOnly {
				continue
			}
			p := on(v, map[profile.Protocol]string{proto: hosts[proto]})
			e.credential(p, v.keyEnv, 1)
			for _, m := range v.models {
				tests = append(tests, tested{v: v, d: e.deployment(p, m), proto: proto})
			}
			if proto != profile.OpenAI {
				continue
			}
			kind := func(k store.Kind) func(*store.Deployment) { return func(d *store.Deployment) { d.Kind = k } }
			switch v.name {
			case "gitee":
				tests = append(tests, tested{v: v, d: e.deployment(p, "Qwen3-Embedding-0.6B", kind(store.KindEmbedding)), proto: proto},
					tested{v: v, d: e.deployment(p, "bge-reranker-v2-m3", kind(store.KindRerank)), proto: proto})
			case "zhipu":
				tests = append(tests, tested{v: v, d: e.deployment(p, "embedding-3", kind(store.KindEmbedding)), proto: proto})
			}
		}
	}
	e.startProbing()

	for _, c := range checks {
		t.Run("check/"+c.v.name, func(t *testing.T) {
			prof, _ := profile.Lookup(c.v.name)
			for _, r := range e.check(c.p.ID, c.good.ID) {
				listed := prof.ModelList[r.Protocol]
				switch {
				case !listed && (r.OK || r.Status != nil || r.Error == nil || r.Error.Type != "not_found_error"):
					liveErrorf(t, "%s, which serves no list: %s", r.Protocol, encodeJSON(r))
				case listed && (!r.OK || len(r.Models) == 0):
					liveErrorf(t, "%s: %s", r.Protocol, encodeJSON(r))
				case listed:
					for _, m := range c.v.models {
						if !slices.Contains(r.Models, m) {
							liveErrorf(t, "%s lists no %s among %v", r.Protocol, m, r.Models)
						}
					}
					t.Logf("%s lists %d models in %dms", r.Protocol, len(r.Models), *r.LatencyMS)
				}
			}
			for _, r := range e.check(c.p.ID, c.bad.ID) {
				if prof.ModelList[r.Protocol] && (r.OK || r.Status == nil || r.Error == nil) {
					liveErrorf(t, "%s with a bad key: %s", r.Protocol, encodeJSON(r))
				}
				if r.Status != nil {
					t.Logf("%s refuses a bad key: %d %s %s", r.Protocol, *r.Status, r.Error.Type, masked(r.Error.Message))
				}
			}
		})
	}
	for _, c := range tests {
		t.Run("test/"+c.v.name+"/"+string(c.proto)+"/"+c.d.UpstreamModel, func(t *testing.T) {
			got, _ := e.test(c.d.ID, "")
			if len(got) != 1 {
				liveFatalf(t, "results: %s", encodeJSON(got))
			}
			r := got[0]
			if !r.OK || r.Status == nil || *r.Status != 200 || r.Protocol == nil || *r.Protocol != c.proto ||
				c.d.Kind != store.KindRerank && (r.Usage == nil || r.Usage.InputTokens+r.Usage.CacheReadTokens == 0) ||
				c.d.Kind == store.KindChat && r.Usage.OutputTokens == 0 {
				liveErrorf(t, "%s", masked(encodeJSON(r)))
			}
			t.Logf("%dms, usage %s", *r.LatencyMS, encodeJSON(r.Usage))
		})
	}
	n := 0
	for _, row := range e.ledger() {
		if row.Endpoint == "deployment_test" && row.APIKeyID == "admin" {
			n++
		}
	}
	if n != len(tests) {
		t.Errorf("the ledger holds %d test rows, want %d", n, len(tests))
	}
}

// liveHosts is each protocol's host in v's profile, in the region of the
// tier's base URL for v.
func liveHosts(t *testing.T, v liveVendor) map[profile.Protocol]string {
	t.Helper()
	prof, _ := profile.Lookup(v.name)
	i := slices.IndexFunc(prof.Hosts, func(h profile.Host) bool {
		return strings.TrimRight(h.BaseURL, "/") == strings.TrimRight(v.base, "/")
	})
	if i < 0 {
		t.Fatalf("%s: %s is none of its profile's hosts", v.name, v.base)
	}
	out := map[profile.Protocol]string{}
	for _, h := range prof.Hosts {
		if h.Region == prof.Hosts[i].Region {
			out[h.Protocol] = h.BaseURL
		}
	}
	return out
}
