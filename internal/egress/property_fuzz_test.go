package egress

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func FuzzValidateAndCanonicalHostEntry(f *testing.F) {
	for _, seed := range []string{
		"api.example.com",
		"*.example.com",
		"API.EXAMPLE.COM.",
		"*.Sub.Example.Com.",
		"münchen.de",
		"xn--mnchen-3ya.de",
		"127.0.0.1",
		"[::1]",
		"example.com:443",
		"",
		"*",
		"*.",
		"..example.com",
		"a..b.com",
		"has space.com",
		"foo\x00bar.com",
		strings.Repeat("a", maxCanonicalHost+1),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		// ValidateHostEntry and CanonicalEntry must never panic.
		err := ValidateHostEntry(raw)
		canon, cerr := CanonicalEntry(raw)
		if err == nil {
			// A valid host entry must be pure ASCII (#614).
			for i := 0; i < len(raw); i++ {
				if raw[i] >= 0x80 {
					t.Fatalf("ValidateHostEntry accepted non-ASCII host %q", raw)
				}
			}
			if cerr != nil {
				t.Fatalf("ValidateHostEntry(%q) == nil, but CanonicalEntry failed: %v", raw, cerr)
			}
			// CanonicalEntry must be idempotent.
			canon2, err2 := CanonicalEntry(canon)
			if err2 != nil || canon2 != canon {
				t.Fatalf("CanonicalEntry not idempotent on %q: %q -> %q (%v)", raw, canon, canon2, err2)
			}
			// HostSet deliberately refuses hosts past maxCanonicalHost (1024 bytes).
			// Within that lookup cap, policy coverage must be reflexive; past it,
			// oversized entries must fail closed rather than match.
			hs := NewHostSet([]string{raw})
			stripped := strings.TrimPrefix(strings.TrimSpace(raw), "*.")
			if len(stripped) <= maxCanonicalHost {
				if !hs.CoversEntry(raw) {
					t.Fatalf("HostSet(%q) does not cover itself", raw)
				}
				if !hs.CoversEntry(canon) {
					t.Fatalf("HostSet(%q) does not cover its canonical form %q", raw, canon)
				}
				if !NetworkingCovers(false, []string{raw}, false, []string{raw}) {
					t.Fatalf("NetworkingCovers not reflexive on %q", raw)
				}
			} else if hs.CoversEntry(raw) {
				t.Fatalf("HostSet accepted over-cap entry of length %d", len(stripped))
			}
		}
		// NetworkingBindingDigest must always emit a 64-hex digest and be order/dup invariant.
		d1 := NetworkingBindingDigest(false, []string{raw})
		d2 := NetworkingBindingDigest(false, []string{raw, raw})
		if len(d1) != 64 || d1 != d2 {
			t.Fatalf("NetworkingBindingDigest(%q) len=%d dup-equal=%v", raw, len(d1), d1 == d2)
		}
	})
}

func TestNetworkingBindingDigestOrderAndDedupInvariant(t *testing.T) {
	pool := []string{
		"api.github.com",
		"API.GITHUB.COM.",
		"*.openai.com",
		"*.OPENAI.COM",
		"registry.npmjs.org",
		"pypi.org",
		"files.pythonhosted.org",
		"malformed..host",
	}
	rng := rand.New(rand.NewPCG(75, 30))
	for range 100 {
		n := rng.IntN(len(pool)) + 1
		base := make([]string, n)
		for i := range n {
			base[i] = pool[rng.IntN(len(pool))]
		}
		shuffled := slices.Clone(base)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		duped := append(slices.Clone(shuffled), base[0])

		dBase := NetworkingBindingDigest(false, base)
		dShuf := NetworkingBindingDigest(false, shuffled)
		dDup := NetworkingBindingDigest(false, duped)
		if dBase != dShuf || dBase != dDup {
			t.Fatalf("digest varied across permutation/dedup: base=%v -> %s, shuf=%v -> %s, dup=%v -> %s",
				base, dBase, shuffled, dShuf, duped, dDup)
		}
		// Unrestricted digest must never collide with any limited digest.
		if dBase == NetworkingBindingDigest(true, nil) {
			t.Fatalf("limited digest collided with unrestricted digest for %v", base)
		}
	}
}
