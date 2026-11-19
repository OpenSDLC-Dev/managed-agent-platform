package egress

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
)

// SealedApprovedNetworkingKey is the key inside an environment_variable
// credential's sealed secret JSON holding the canonical SHA-256 digest of the
// networking destinations approved for that secret material (plan 70 slice 1).
const SealedApprovedNetworkingKey = "approved_networking_sha256"

// NetworkingCovers reports whether every host admitted by the candidate
// destination policy (nextUnrestricted, nextHosts) is already admitted by the
// existing destination policy (existingUnrestricted, existingHosts).
//
// An unrestricted existing policy covers every valid candidate policy. A
// limited existing policy never covers unrestricted, never covers an empty
// candidate list (fail-closed), and covers a limited candidate iff its HostSet
// covers every candidate entry via HostSet.CoversEntry.
func NetworkingCovers(existingUnrestricted bool, existingHosts []string, nextUnrestricted bool, nextHosts []string) bool {
	if existingUnrestricted {
		return true
	}
	if nextUnrestricted || len(nextHosts) == 0 {
		return false
	}
	allowed := NewHostSet(existingHosts)
	for _, entry := range nextHosts {
		if !allowed.CoversEntry(entry) {
			return false
		}
	}
	return true
}

// NetworkingBindingDigest returns a deterministic 64-hex-character SHA-256
// digest over a credential's destination policy. Storing this fixed-size digest
// inside the cipher-authenticated secret JSON binds the secret material to its
// approved destinations without scaling plaintext size with allowed_hosts.
//
// Valid entries are canonicalized the same way NewHostSet keys them (trimmed,
// canonicalized, de-rooted, with "*." distinguished from exact hosts), sorted,
// and deduplicated so equivalent spellings produce one digest. Malformed
// entries are kept under a disjoint prefix rather than dropped so a tampered
// row cannot match a clean policy.
func NetworkingBindingDigest(unrestricted bool, allowedHosts []string) string {
	h := sha256.New()
	if unrestricted {
		h.Write([]byte("unrestricted\n"))
		return hex.EncodeToString(h.Sum(nil))
	}
	norm := make([]string, 0, len(allowedHosts))
	for _, raw := range allowedHosts {
		norm = append(norm, canonicalBindingEntry(raw))
	}
	slices.Sort(norm)
	norm = slices.Compact(norm)
	h.Write([]byte("limited"))
	for _, entry := range norm {
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(len(entry))))
		h.Write([]byte{':'})
		h.Write([]byte(entry))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalBindingEntry(raw string) string {
	e := strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(e, "*."); ok {
		if len(rest) <= maxCanonicalHost {
			if rest = canonicalDeRooted(rest); rest != "" && !hasEmptyLabel(rest) {
				return "w:" + rest
			}
		}
		return "x:" + raw
	}
	if len(e) <= maxCanonicalHost {
		if c := canonicalDeRooted(e); c != "" && !hasEmptyLabel(c) {
			return "e:" + c
		}
	}
	return "x:" + raw
}
