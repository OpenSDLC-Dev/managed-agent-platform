package identity

import "strings"

// claimAt resolves a configured claim name against a decoded claim set.
//
// Three cases, and which one applies is decided by the CONFIGURED NAME alone:
//
//   - a name with no dot is a single map key;
//   - a name that is URI-shaped (it contains "://") is a single map key too,
//     dots and all — this is the namespaced-custom-claim convention Auth0
//     requires and Okta and Entra also use, e.g.
//     "https://corp.example/roles". Splitting it on "." would walk
//     ["https://corp", "example", "com/roles"], find nothing, and deny every
//     human on those providers with nothing in any log to say why;
//   - any other dotted name is a PATH, walked segment by segment — the Keycloak
//     shape, "resource_access.console.roles".
//
// Deciding from the name and never from the token is the security property. The
// alternative shape, "try the flat key and fall back to walking", lets the TOKEN
// choose the interpretation: an IdP surface that lets a user place a flat claim
// literally named "resource_access.console.roles" — a self-service attribute, a
// mapper over a user-editable profile field — would silently outrank the real
// nested claim, and the user would map their own role. Here the operator's
// configured name fixes the reading before any token is seen, so no claim a
// token carries can switch it.
func claimAt(claims map[string]any, name string) any {
	if claims == nil || name == "" {
		return nil
	}
	if !strings.Contains(name, ".") || strings.Contains(name, "://") {
		return claims[name]
	}
	parts := strings.Split(name, ".")
	if len(parts) > maxClaimDepth {
		return nil
	}
	var cur any = claims
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = m[p]; !ok {
			return nil
		}
	}
	return cur
}

// stringClaim resolves name and returns it only when it decoded as a string. A
// number, bool, array or object yields "" — never an error: a missing or
// oddly-typed email is not an authentication failure.
func stringClaim(claims map[string]any, name string) string {
	s, _ := claimAt(claims, name).(string)
	return s
}

// roleValues normalizes a resolved roles claim to its string values.
//
// A scalar string is one value — NOT split on spaces, which would be inventing
// OAuth scope semantics nobody asked for. An array contributes its string
// elements and silently drops the rest. Anything else contributes none.
//
// The cap bounds the elements EXAMINED, not the strings collected. Capping the
// output instead would let a claim pad itself past the limit with non-strings
// and still be read at any depth, which is the whole cap defeated; and where the
// two differ, this direction drops a role rather than granting one.
func roleValues(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		if len(t) > maxRoleValues {
			t = t[:maxRoleValues]
		}
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// strongestRole reduces mapped values to the single strongest role by the fixed
// order admin > developer > viewer. Unmapped values drop; nothing mapped yields
// RoleNone.
//
// Reducing by rank rather than by position is the property: neither claim order
// nor Go's randomized map iteration can change the answer.
func strongestRole(values []string, m map[string]Role) Role {
	best := RoleNone
	for _, v := range values {
		if r, ok := m[v]; ok && roleRank[r] > roleRank[best] {
			best = r
		}
	}
	return best
}
