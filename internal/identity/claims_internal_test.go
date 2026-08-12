package identity

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// claimsXDecode decodes a JSON literal into a claim set.
//
// The tests below go through real JSON rather than hand-built Go maps because
// that is the only shape claimAt ever sees in production: the verifier decodes
// the JWS payload into a map[string]any, so a number arrives as float64, an
// array as []any, an object as map[string]any and a JSON null as an untyped nil.
// Writing the fixtures as documents keeps the tests honest about all four.
func claimsXDecode(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("fixture claim set does not decode as JSON: %v", err)
	}
	return m
}

// claimsXNest builds a claim set nesting leaf under segments, so a depth-cap row
// can point at a path that a walking implementation would genuinely resolve.
func claimsXNest(segments []string, leaf any) map[string]any {
	cur := leaf
	for i := len(segments) - 1; i >= 0; i-- {
		cur = map[string]any{segments[i]: cur}
	}
	return cur.(map[string]any)
}

// claimsXSegments returns n path segments, s0 … s(n-1).
func claimsXSegments(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("s%d", i)
	}
	return out
}

func TestClaimAtFlatKey(t *testing.T) {
	t.Parallel()
	claims := claimsXDecode(t, `{"roles": ["eng"], "email": "a@example.com", "nothing": null}`)
	for _, tc := range []struct {
		name   string
		claims map[string]any
		claim  string
		want   any
	}{
		{name: "array value", claims: claims, claim: "roles", want: []any{"eng"}},
		{name: "string value", claims: claims, claim: "email", want: "a@example.com"},
		{name: "present but null", claims: claims, claim: "nothing", want: nil},
		{name: "absent key", claims: claims, claim: "missing", want: nil},
		{name: "nil claim set", claims: nil, claim: "roles", want: nil},
		{name: "empty claim name", claims: claims, claim: "", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := claimAt(tc.claims, tc.claim); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("claimAt(%q) = %#v, want %#v", tc.claim, got, tc.want)
			}
		})
	}
}

// TestClaimAtDottedPathOnly pins the escalation fix: a dotted claim name is a
// PATH and never additionally a literal key.
//
// The obvious ordering — exact key first, walk as a fallback — is the vulnerable
// one. With a configured roles claim of resource_access.console.roles, any IdP
// surface that lets a user place a flat claim literally named
// "resource_access.console.roles" (a self-service attribute, a mapper over a
// user-editable profile field) would outrank the real nested claim, and the user
// would map their own role. Path-only turns that escalation into a denial.
func TestClaimAtDottedPathOnly(t *testing.T) {
	t.Parallel()

	t.Run("nested wins over a flat key of the same name", func(t *testing.T) {
		t.Parallel()
		claims := claimsXDecode(t, `{
			"a.b": "flat",
			"a": {"b": "nested"}
		}`)
		if got := claimAt(claims, "a.b"); got != "nested" {
			t.Errorf("claimAt(%q) = %#v, want the nested value %q — a flat key of the "+
				"same name must never outrank the real nested claim", "a.b", got, "nested")
		}
	})

	t.Run("Keycloak shape, flat shadow present", func(t *testing.T) {
		t.Parallel()
		// The realistic attack: the attacker-placed flat claim grants admin, the
		// genuine nested claim grants viewer. Resolving the flat one is privilege
		// escalation.
		claims := claimsXDecode(t, `{
			"resource_access.console.roles": ["platform-admin"],
			"resource_access": {"console": {"roles": ["platform-viewer"]}}
		}`)
		roles := map[string]Role{"platform-viewer": RoleViewer, "platform-admin": RoleAdmin}
		got := strongestRole(roleValues(claimAt(claims, "resource_access.console.roles")), roles)
		if got != RoleViewer {
			t.Errorf("role from the shadowed Keycloak claim = %q, want %q — the flat "+
				"claim escalated the user to their own role", got, RoleViewer)
		}
	})

	t.Run("flat key alone resolves to nothing", func(t *testing.T) {
		t.Parallel()
		claims := claimsXDecode(t, `{"a.b": "flat"}`)
		if got := claimAt(claims, "a.b"); got != nil {
			t.Errorf("claimAt(%q) = %#v, want nil — a dotted name resolves only as a path", "a.b", got)
		}
	})
}

// TestClaimAtURINamespacedName covers the convention Auth0 REQUIRES for custom
// claims and Okta and Entra also use. Those names contain dots inside a hostname,
// so treating every dotted name as a path would split
// "https://corp.example/roles" into ["https://corp", "example", "com/roles"],
// resolve nothing, and deny every human on those providers with nothing in any
// log to say why.
//
// The escalation property survives because the reading is chosen by the
// CONFIGURED NAME, never by what the token happens to carry: the last subtest
// shows a token cannot make a URI-shaped name walk, and TestClaimAtDottedPathOnly
// above shows it cannot make a path-shaped name resolve flat.
func TestClaimAtURINamespacedName(t *testing.T) {
	t.Parallel()

	t.Run("https namespaced claim resolves whole", func(t *testing.T) {
		t.Parallel()
		claims := claimsXDecode(t, `{"https://corp.example/roles": ["platform-admins"]}`)
		got := roleValues(claimAt(claims, "https://corp.example/roles"))
		if len(got) != 1 || got[0] != "platform-admins" {
			t.Errorf("claimAt = %#v, want the namespaced claim read as one key", got)
		}
	})

	t.Run("the whole role pipeline, end to end", func(t *testing.T) {
		t.Parallel()
		claims := claimsXDecode(t, `{"https://corp.example/roles": ["platform-admins"]}`)
		roles := map[string]Role{"platform-admins": RoleAdmin}
		if got := strongestRole(roleValues(claimAt(claims, "https://corp.example/roles")), roles); got != RoleAdmin {
			t.Errorf("role = %q, want %q — an Auth0-shaped deployment must not silently deny everyone", got, RoleAdmin)
		}
	})

	t.Run("a token cannot make a URI name walk", func(t *testing.T) {
		t.Parallel()
		// Both readings are present in the token. The URI-shaped configured name
		// takes the flat key, so the crafted nested structure is unreachable.
		claims := claimsXDecode(t, `{
			"https://corp.example/roles": ["viewer-value"],
			"https://corp": {"example/roles": ["admin-value"]}
		}`)
		got := roleValues(claimAt(claims, "https://corp.example/roles"))
		if len(got) != 1 || got[0] != "viewer-value" {
			t.Errorf("claimAt = %#v, want the flat value: a URI-shaped name never walks", got)
		}
	})
}

func TestClaimAtNestedThreeDeep(t *testing.T) {
	t.Parallel()
	// Both shapes a real Keycloak token carries.
	claims := claimsXDecode(t, `{
		"resource_access": {"console": {"roles": ["eng", "sre"]}},
		"realm_access": {"roles": ["offline_access"]}
	}`)
	for _, tc := range []struct {
		name  string
		claim string
		want  any
	}{
		{name: "three deep", claim: "resource_access.console.roles", want: []any{"eng", "sre"}},
		{name: "two deep", claim: "realm_access.roles", want: []any{"offline_access"}},
		{name: "intermediate object itself", claim: "resource_access.console", want: map[string]any{"roles": []any{"eng", "sre"}}},
		{name: "missing leaf", claim: "resource_access.console.groups", want: nil},
		{name: "missing intermediate", claim: "resource_access.portal.roles", want: nil},
		{name: "missing root", claim: "absent.console.roles", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := claimAt(claims, tc.claim); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("claimAt(%q) = %#v, want %#v", tc.claim, got, tc.want)
			}
		})
	}
}

func TestClaimAtNonObjectIntermediate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{name: "string intermediate", doc: `{"a": "b"}`},
		{name: "array intermediate", doc: `{"a": ["b"]}`},
		{name: "number intermediate", doc: `{"a": 7}`},
		{name: "bool intermediate", doc: `{"a": true}`},
		{name: "null intermediate", doc: `{"a": null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claims := claimsXDecode(t, tc.doc)
			if got := claimAt(claims, "a.b"); got != nil {
				t.Errorf("claimAt(%q) over %s = %#v, want nil", "a.b", tc.doc, got)
			}
		})
	}
}

// TestClaimAtDepthCap pins that a path longer than maxClaimDepth resolves to nil
// rather than being walked. The claim set is genuinely that deep, so a walking
// implementation would return the leaf: nil is proof the cap fired.
func TestClaimAtDepthCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		depth   int
		wantNil bool
	}{
		{name: "at the cap", depth: maxClaimDepth},
		{name: "one over the cap", depth: maxClaimDepth + 1, wantNil: true},
		{name: "far over the cap", depth: maxClaimDepth * 4, wantNil: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			segments := claimsXSegments(tc.depth)
			claims := claimsXNest(segments, "leaf")
			got := claimAt(claims, strings.Join(segments, "."))
			if tc.wantNil {
				if got != nil {
					t.Errorf("claimAt over a %d-segment path = %#v, want nil (maxClaimDepth is %d)",
						tc.depth, got, maxClaimDepth)
				}
				return
			}
			if got != "leaf" {
				t.Errorf("claimAt over a %d-segment path = %#v, want %q — the cap fired one segment early",
					tc.depth, got, "leaf")
			}
		})
	}
}

func TestStringClaim(t *testing.T) {
	t.Parallel()
	claims := claimsXDecode(t, `{
		"email": "a@example.com",
		"empty": "",
		"number": 7,
		"bool": true,
		"array": ["a@example.com"],
		"object": {"value": "a@example.com"},
		"null": null,
		"profile": {"name": "Ada"}
	}`)
	for _, tc := range []struct {
		name  string
		claim string
		want  string
	}{
		{name: "string", claim: "email", want: "a@example.com"},
		{name: "empty string", claim: "empty", want: ""},
		{name: "nested string", claim: "profile.name", want: "Ada"},
		{name: "number", claim: "number"},
		{name: "bool", claim: "bool"},
		{name: "array", claim: "array"},
		{name: "object", claim: "object"},
		{name: "null", claim: "null"},
		{name: "absent", claim: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// An oddly-typed or missing email is never an authentication failure:
			// it is an empty field on the Identity.
			if got := stringClaim(claims, tc.claim); got != tc.want {
				t.Errorf("stringClaim(%q) = %q, want %q", tc.claim, got, tc.want)
			}
		})
	}
}

func TestRoleValues(t *testing.T) {
	t.Parallel()
	claims := claimsXDecode(t, `{
		"scalar": "eng",
		"one": ["eng"],
		"mixed": ["eng", 7, true, null, {"x": 1}, ["nested"], "sre"],
		"empty": [],
		"null": null,
		"number": 7,
		"bool": true,
		"object": {"roles": ["eng"]}
	}`)
	for _, tc := range []struct {
		name  string
		claim string
		want  []string
	}{
		{name: "scalar string is one value", claim: "scalar", want: []string{"eng"}},
		{name: "single-element array", claim: "one", want: []string{"eng"}},
		{name: "non-string elements drop silently", claim: "mixed", want: []string{"eng", "sre"}},
		{name: "empty array", claim: "empty"},
		{name: "absent", claim: "missing"},
		{name: "null", claim: "null"},
		{name: "number", claim: "number"},
		{name: "bool", claim: "bool"},
		{name: "object", claim: "object"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := roleValues(claimAt(claims, tc.claim))
			if !slices.Equal(got, tc.want) {
				t.Errorf("roleValues(%q) = %#v, want %#v", tc.claim, got, tc.want)
			}
		})
	}
}

func TestRoleValuesCap(t *testing.T) {
	t.Parallel()
	const total = 200
	raw := make([]any, total)
	for i := range raw {
		raw[i] = fmt.Sprintf("r%d", i)
	}
	got := roleValues(raw)
	if len(got) != maxRoleValues {
		t.Fatalf("roleValues over %d elements returned %d values, want the cap %d",
			total, len(got), maxRoleValues)
	}
	// The cap reads a prefix, in document order, rather than an arbitrary subset.
	for i, v := range got {
		if want := fmt.Sprintf("r%d", i); v != want {
			t.Fatalf("roleValues[%d] = %q, want %q", i, v, want)
		}
	}
}

// TestRoleValuesCapCountsElementsNotStrings is the cap's real question, and the
// two readings differ in the direction that matters. Counting only the strings
// COLLECTED would let a claim pad itself past the limit with values that cost a
// loop iteration each and are not strings — nulls here — and still be read at
// any depth: the cap defeated by construction, with the padded-past value being
// the one that grants admin. Counting elements EXAMINED bounds the work as
// stated, and where the two disagree it drops a role rather than granting one.
func TestRoleValuesCapCountsElementsNotStrings(t *testing.T) {
	t.Parallel()

	raw := make([]any, 0, maxRoleValues+1)
	for range maxRoleValues {
		raw = append(raw, nil)
	}
	raw = append(raw, "platform-admins")

	if got := roleValues(raw); len(got) != 0 {
		t.Fatalf("roleValues = %q, want nothing: the string sits past the cap", got)
	}
	// The control: one fewer pad and the same value is inside the cap and read.
	if got := roleValues(raw[1:]); len(got) != 1 || got[0] != "platform-admins" {
		t.Fatalf("roleValues = %q, want the value that sits inside the cap", got)
	}
}

// TestSpaceDelimitedScalarIsOneValue pins that a scalar roles claim is never
// split on spaces. Splitting would invent OAuth `scope` semantics the design
// never asked for, and it would let the single claim value "eng admin" match a
// role map entry for "admin".
func TestSpaceDelimitedScalarIsOneValue(t *testing.T) {
	t.Parallel()
	got := roleValues("eng admin")
	if want := []string{"eng admin"}; !slices.Equal(got, want) {
		t.Errorf("roleValues(%q) = %#v, want %#v — the scalar was split on spaces", "eng admin", got, want)
	}
	if r := strongestRole(got, map[string]Role{"admin": RoleAdmin, "eng": RoleViewer}); r != RoleNone {
		t.Errorf("strongestRole over the unsplit scalar = %q, want %q — a space-delimited "+
			"value matched a role map entry it does not equal", r, RoleNone)
	}
}

// TestStrongestRoleIsOrderIndependent pins that the reduction is by rank, so
// neither claim order nor Go's randomized map iteration can change the answer.
func TestStrongestRoleIsOrderIndependent(t *testing.T) {
	t.Parallel()
	m := map[string]Role{"eng": RoleViewer, "sre": RoleAdmin, "build": RoleDeveloper}
	// Every permutation of one viewer-mapped, one admin-mapped and one unmapped
	// value. The strongest is admin in all six.
	for _, values := range [][]string{
		{"eng", "sre", "unmapped"},
		{"eng", "unmapped", "sre"},
		{"sre", "eng", "unmapped"},
		{"sre", "unmapped", "eng"},
		{"unmapped", "eng", "sre"},
		{"unmapped", "sre", "eng"},
	} {
		t.Run(fmt.Sprint(values), func(t *testing.T) {
			t.Parallel()
			// Repeated, because map iteration order is randomized per range: a
			// single pass over an implementation that reduced by iteration order
			// would pass by luck often enough to look green.
			for i := 0; i < 100; i++ {
				if got := strongestRole(values, m); got != RoleAdmin {
					t.Fatalf("strongestRole(%v) = %q on iteration %d, want %q", values, got, i, RoleAdmin)
				}
			}
		})
	}
	// Developer beats viewer for the same reason, so the order is a full order
	// and not just "admin wins".
	for i := 0; i < 100; i++ {
		if got := strongestRole([]string{"build", "eng"}, m); got != RoleDeveloper {
			t.Fatalf("strongestRole([build eng]) = %q on iteration %d, want %q", got, i, RoleDeveloper)
		}
	}
}

func TestStrongestRoleUnmappedDrop(t *testing.T) {
	t.Parallel()
	m := map[string]Role{"everyone": RoleViewer}
	for _, tc := range []struct {
		name   string
		values []string
		roles  map[string]Role
		want   Role
	}{
		{name: "unmapped values drop", values: []string{"unknown", "everyone"}, roles: m, want: RoleViewer},
		{name: "nothing mapped", values: []string{"unknown", "other"}, roles: m, want: RoleNone},
		{name: "no values", values: nil, roles: m, want: RoleNone},
		{name: "empty role map", values: []string{"everyone"}, roles: map[string]Role{}, want: RoleNone},
		{name: "nil role map", values: []string{"everyone"}, roles: nil, want: RoleNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := strongestRole(tc.values, tc.roles); got != tc.want {
				t.Errorf("strongestRole(%v) = %q, want %q", tc.values, got, tc.want)
			}
		})
	}
}
