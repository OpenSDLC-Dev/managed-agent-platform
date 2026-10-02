// Package unknownkey picks the unknown key a strict decoder's refusal names.
// It is its own leaf, stdlib only, because three packages refuse unknown keys
// in the reference's manner and must pick the same one — internal/api's
// request bodies, internal/toolset's configs and internal/events' inbound
// events — and none of them may import another for it, while internal/domain
// holds the Anthropic-native types and nothing else.
package unknownkey

import "slices"

// Least reports the key of obj outside allowed that a refusal of unknown keys
// names, if there is one: the least in byte order, so a body with several
// names the same one on every request. The reference's choice agrees on the
// one recorded body with several (2026-09-03 batch2 `cred.create.oauth-te-400`
// named access_token among five).
func Least[V any](obj map[string]V, allowed ...string) (unknown string, found bool) {
	for key := range obj {
		if !slices.Contains(allowed, key) && (!found || key < unknown) {
			unknown, found = key, true
		}
	}
	return unknown, found
}
