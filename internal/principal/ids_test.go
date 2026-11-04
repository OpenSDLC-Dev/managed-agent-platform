package principal

import (
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

func TestPrivatePrincipalAliases(t *testing.T) {
	for _, tc := range []struct{ internal, kind, public string }{
		{"principal_abc123", "user", "user_abc123"},
		{"principal_abc123", "service_account", "svac_abc123"},
		{"principal_0123456789abcdefghjkmnpqr", "user", "user_0123456789abcdefghjkmnpqr"},
	} {
		if got := PublicID(tc.internal, tc.kind); got != tc.public {
			t.Errorf("PublicID(%q,%q)=%q,want %q", tc.internal, tc.kind, got, tc.public)
		}
		id, kind, ok := ParsePublicID(tc.public)
		if !ok || id != tc.internal || kind != tc.kind {
			t.Errorf("ParsePublicID(%q)=(%q,%q,%v)", tc.public, id, kind, ok)
		}
		if domain.ID(tc.public).Valid() {
			t.Fatalf("private alias %q widened public domain prefixes", tc.public)
		}
	}
	for _, value := range []string{"", "principal_abc123", "user_", "svac_", "user_abc/123", "user_abc_123", "user_abc\x00", "user_abc%31", "user_é", "agent_abc123"} {
		if _, _, ok := ParsePublicID(value); ok {
			t.Errorf("accepted malformed/wrong private alias %q", value)
		}
	}
	for _, tc := range []struct{ id, kind string }{{"principal_abc123", "other"}, {"user_abc123", "user"}, {"principal_", "user"}, {"principal_abc/123", "user"}} {
		if got := PublicID(tc.id, tc.kind); got != "" {
			t.Errorf("invalid principal converted to %q", got)
		}
	}
}
