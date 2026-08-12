package identity

import "strings"

// claimAt resolves a configured claim name against a decoded claim set.
//
// A name with no dot is a single map key. A name containing a dot resolves ONLY
// as a path — never additionally as a literal key of that name.
//
// That ordering is the security-relevant one, and it is the reverse of the
// obvious "exact key first, walk as a fallback". With a configured claim name of
// resource_access.console.roles (the Keycloak shape), any IdP surface that lets a
// user place a FLAT claim literally named "resource_access.console.roles" — a
// self-service attribute, a mapper over a user-editable profile field — would
// silently outrank the real nested claim, and the user would map their own role.
// Path-only turns that escalation into a denial, and costs nothing: a dotless
// name is unambiguous and still takes the flat lookup.
func claimAt(claims map[string]any, name string) any {
	if claims == nil || name == "" {
		return nil
	}
	if !strings.Contains(name, ".") {
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
