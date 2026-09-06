package identity

import (
	"slices"
	"strings"
)

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

// claimNameTooDeep reports a configured name that claimAt would refuse to walk.
// It lives here, immediately beside the rule it mirrors, because the two must
// agree on what counts as a path: a URI-shaped name is one flat key however many
// dots it carries, so no Auth0-style namespaced claim can trip this.
//
// New calls it so that an over-deep name is a BOOT error. Without that check the
// configuration is accepted, claimAt resolves to nil on every request, and the
// deployment maps every human to RoleNone — a control plane that denies everyone
// with nothing in any log to say why, which is the worst shape a configuration
// defect can take.
func claimNameTooDeep(name string) bool {
	if !strings.Contains(name, ".") || strings.Contains(name, "://") {
		return false
	}
	return strings.Count(name, ".")+1 > maxClaimDepth
}

// stringClaim resolves name and returns it only when it decoded as a string. A
// number, bool, array or object yields "" — never an error: a missing or
// oddly-typed email is not an authentication failure.
func stringClaim(claims map[string]any, name string) string {
	s, _ := claimAt(claims, name).(string)
	return s
}

// claimValues normalizes a resolved multi-valued claim to its string values. It
// serves both of them — the roles claim and, since plan 42 §6.2, the workspaces
// claim — because the normalization is the same question either way and a
// second copy of it could drift on the cap.
//
// A scalar string is one value — NOT split on spaces, which would be inventing
// OAuth scope semantics nobody asked for. An array contributes its string
// elements and silently drops the rest. Anything else contributes none.
//
// The cap bounds the elements EXAMINED, not the strings collected. Capping the
// output instead would let a claim pad itself past the limit with non-strings
// and still be read at any depth, which is the whole cap defeated; and where the
// two differ, this direction drops a value rather than granting one.
func claimValues(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		if len(t) > maxClaimValues {
			t = t[:maxClaimValues]
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

// mappedWorkspaces reduces claim values to the workspace ids the operator's map
// binds them to, in claim order and without repeats — two IdP groups may name
// one workspace, and the identity covers it once.
//
// An unmapped value DROPS, exactly as it does in strongestRole, and dropping is
// the safer of the two readings rather than the lazier one. Every real IdP sends
// groups a deployment has no interest in, so refusing a token that carries one
// would deny every human on it — a denial with no diagnostic, which is the worst
// shape this package's configuration defects can take. Dropping cannot widen
// anything: a value nobody mapped names no workspace, and an identity that maps
// to none resolves to no workspace at all, which the identity lane refuses.
func mappedWorkspaces(values []string, m map[string]string) []string {
	var out []string
	for _, v := range values {
		id, ok := m[v]
		if !ok || slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
	}
	return out
}
