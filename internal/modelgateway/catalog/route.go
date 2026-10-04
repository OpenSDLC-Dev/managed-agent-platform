package catalog

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"slices"
	"sort"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// Attempt is one upstream call a request may make: one deployment, on its
// provider, with one of the provider's credentials, at the provider's
// endpoint for the request's protocol.
type Attempt struct {
	Deployment store.Deployment
	Provider   store.Provider
	Credential store.Credential
	Endpoint   string
}

// Plan orders the attempts a request for a may make on proto
// (docs/plan/59_model-gateway.md, "Routing, retries, limits"): the priority
// groups in ascending order; within a group, each eligible deployment in
// weighted order; within a deployment, each eligible credential in weighted
// order. The caller tries them in turn, so a failure moves to the next
// credential, then the next deployment, then the next group.
//
// Eligible means enabled all the way down — the deployment, its provider and
// the credential — with an endpoint on proto and the credential allowed on it.
// A deployment with no eligible credential is not a target.
//
// The weighted order is the exponential-key form of weighted sampling without
// replacement: each candidate draws u in (0, 1] and sorts by -ln(u)/weight, so
// it comes first with probability proportional to its weight. Without a
// session, u is draw's; with one, u is a hash of the session and the
// candidate's id — weighted rendezvous hashing — so a session's requests keep
// one deployment and one credential while the candidates and their weights do
// not change, and keep reaching one upstream prompt cache. Nothing correct
// depends on that stickiness.
func (s *Snapshot) Plan(a store.Alias, proto profile.Protocol, session string, draw func() float64) []Attempt {
	byPriority := map[int][]store.Target{}
	for _, t := range a.Targets {
		byPriority[t.Priority] = append(byPriority[t.Priority], t)
	}
	priorities := make([]int, 0, len(byPriority))
	for p := range byPriority {
		priorities = append(priorities, p)
	}
	sort.Ints(priorities)

	var out []Attempt
	for _, p := range priorities {
		type target struct {
			d     store.Deployment
			prov  store.Provider
			creds []store.Credential
			w     int
		}
		var group []target
		for _, t := range byPriority[p] {
			d, ok := s.deployments[t.DeploymentID]
			if !ok || !d.Enabled {
				continue
			}
			prov, ok := s.providers[d.ProviderID]
			if !ok || !prov.Enabled || prov.Endpoints[proto] == "" {
				continue
			}
			var creds []store.Credential
			for _, c := range s.credentials[prov.ID] {
				if c.Enabled && slices.Contains(c.Protocols, proto) {
					creds = append(creds, c)
				}
			}
			if len(creds) == 0 {
				continue
			}
			group = append(group, target{d: d, prov: prov, creds: creds, w: t.Weight})
		}
		weighted(group, session, draw, func(t target) (string, int) { return t.d.ID, t.w })
		for _, t := range group {
			weighted(t.creds, session, draw, func(c store.Credential) (string, int) { return c.ID, c.Weight })
			for _, c := range t.creds {
				out = append(out, Attempt{Deployment: t.d, Provider: t.prov, Credential: c, Endpoint: t.prov.Endpoints[proto]})
			}
		}
	}
	return out
}

// weighted sorts xs in place into the weighted order Plan describes.
func weighted[T any](xs []T, session string, draw func() float64, key func(T) (id string, weight int)) {
	keys := make(map[string]float64, len(xs))
	for _, x := range xs {
		id, w := key(x)
		var u float64
		if session != "" {
			u = hashUnit(session, id)
		} else {
			u = draw()
		}
		keys[id] = -math.Log(u) / float64(max(w, 1))
	}
	sort.SliceStable(xs, func(i, j int) bool {
		a, _ := key(xs[i])
		b, _ := key(xs[j])
		if keys[a] != keys[b] {
			return keys[a] < keys[b]
		}
		return a < b
	})
}

// hashUnit maps a session and a candidate id to a number in (0, 1), the same
// on every replica. SHA-256 rather than a fast hash: FNV-1a's hashes of two
// ids differing in their last byte differ by a constant whatever the session,
// so one candidate would win every session.
func hashUnit(session, id string) float64 {
	sum := sha256.Sum256([]byte(session + "\x00" + id))
	return (float64(binary.BigEndian.Uint64(sum[:8])>>11) + 0.5) / (1 << 53)
}
