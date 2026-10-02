package domain

import "slices"

// LeastUnknownKey reports the key of obj outside allowed that a refusal of
// unknown keys names, if there is one: the least in byte order, so a body with
// several names the same one on every request. The reference's choice agrees
// on the one recorded body with several (2026-09-03 batch2
// `cred.create.oauth-te-400` named access_token among five). The API's
// request bodies, the toolset's configs and the inbound events all refuse an
// unknown key through it.
func LeastUnknownKey[V any](obj map[string]V, allowed ...string) (unknown string, found bool) {
	for key := range obj {
		if !slices.Contains(allowed, key) && (!found || key < unknown) {
			unknown, found = key, true
		}
	}
	return unknown, found
}
