package principal

import (
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// PublicID renders a private member/service-account alias without changing the
// stored principal ID or the audit references that already use it. An invalid
// ID or kind has no alias. Neither prefix enters domain's public resource set.
func PublicID(id, kind string) string {
	if !domain.WellFormedID(id, domain.PrefixPrincipal) {
		return ""
	}
	var prefix string
	switch kind {
	case "user":
		prefix = "user"
	case "service_account":
		prefix = "svac"
	default:
		return ""
	}
	return prefix + "_" + strings.TrimPrefix(id, "principal_")
}

// ParsePublicID decodes an alias's asserted kind. Callers must read the row and
// check its real kind before mutation; an alias is not proof of account type.
func ParsePublicID(value string) (internalID, kind string, ok bool) {
	prefix, suffix, found := strings.Cut(value, "_")
	if !found {
		return "", "", false
	}
	switch prefix {
	case "user":
		kind = "user"
	case "svac":
		kind = "service_account"
	default:
		return "", "", false
	}
	id := "principal_" + suffix
	if !domain.WellFormedID(id, domain.PrefixPrincipal) {
		return "", "", false
	}
	return id, kind, true
}
