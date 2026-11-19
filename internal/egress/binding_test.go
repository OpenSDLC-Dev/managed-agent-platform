package egress

import "testing"

func TestNetworkingCovers(t *testing.T) {
	cases := []struct {
		name      string
		oldUnrest bool
		oldHosts  []string
		newUnrest bool
		newHosts  []string
		want      bool
	}{
		{"unrestricted to unrestricted", true, nil, true, nil, true},
		{"unrestricted to limited", true, nil, false, []string{"api.example.com"}, true},
		{"limited to unrestricted", false, []string{"api.example.com"}, true, nil, false},
		{"limited to empty fails closed", false, []string{"api.example.com"}, false, nil, false},
		{"identical exact host", false, []string{"api.example.com"}, false, []string{"api.example.com"}, true},
		{"case and trailing dot equivalence", false, []string{"api.example.com"}, false, []string{"API.EXAMPLE.COM."}, true},
		{"subset narrowing", false, []string{"a.example.com", "b.example.com"}, false, []string{"a.example.com"}, true},
		{"adding new host expands", false, []string{"a.example.com"}, false, []string{"a.example.com", "b.example.com"}, false},
		{"replacing host expands", false, []string{"a.example.com"}, false, []string{"b.example.com"}, false},
		{"wildcard narrows to exact subdomain", false, []string{"*.example.com"}, false, []string{"api.example.com"}, true},
		{"wildcard narrows to nested wildcard", false, []string{"*.example.com"}, false, []string{"*.sub.example.com"}, true},
		{"exact cannot widen to wildcard", false, []string{"api.example.com"}, false, []string{"*.example.com"}, false},
		{"narrow wildcard cannot widen to parent wildcard", false, []string{"*.sub.example.com"}, false, []string{"*.example.com"}, false},
		{"wildcard does not cover apex", false, []string{"*.example.com"}, false, []string{"example.com"}, false},
		{"malformed candidate fails closed", false, []string{"*.example.com"}, false, []string{".example.com"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NetworkingCovers(tc.oldUnrest, tc.oldHosts, tc.newUnrest, tc.newHosts); got != tc.want {
				t.Fatalf("NetworkingCovers(%v, %v, %v, %v) = %v, want %v",
					tc.oldUnrest, tc.oldHosts, tc.newUnrest, tc.newHosts, got, tc.want)
			}
		})
	}
}

func TestNetworkingBindingDigest(t *testing.T) {
	unrest := NetworkingBindingDigest(true, nil)
	if len(unrest) != 64 {
		t.Fatalf("digest length = %d, want 64", len(unrest))
	}
	if got := NetworkingBindingDigest(true, []string{"ignored.example.com"}); got != unrest {
		t.Fatalf("unrestricted digest varied with ignored hosts: %q vs %q", got, unrest)
	}
	limA := NetworkingBindingDigest(false, []string{"api.example.com", "*.sub.example.com"})
	limB := NetworkingBindingDigest(false, []string{"*.SUB.EXAMPLE.COM.", "API.example.com", "api.example.com"})
	if limA != limB {
		t.Fatalf("canonical equivalent policies produced different digests: %q vs %q", limA, limB)
	}
	if limA == unrest {
		t.Fatal("limited and unrestricted policies collided")
	}
	limAdded := NetworkingBindingDigest(false, []string{"api.example.com", "*.sub.example.com", "other.example.com"})
	if limA == limAdded {
		t.Fatal("adding a host did not change the binding digest")
	}
	// A malformed entry must change the digest rather than being silently dropped.
	limMalformed := NetworkingBindingDigest(false, []string{"api.example.com", "*.sub.example.com", "bad..host"})
	if limA == limMalformed {
		t.Fatal("malformed entry was silently dropped from binding digest")
	}
	limMalformedWild := NetworkingBindingDigest(false, []string{"api.example.com", "*.sub.example.com", "*."})
	if limA == limMalformedWild {
		t.Fatal("malformed wildcard was silently dropped from binding digest")
	}
	// A literal entry starting with the internal prefix cannot collide with a malformed entry.
	a := NetworkingBindingDigest(false, []string{"x:bad..host"})
	b := NetworkingBindingDigest(false, []string{"bad..host"})
	if a == b {
		t.Fatal("disjoint prefixes collided between valid and malformed entries")
	}
}
