package modelgateway

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"slices"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// caller is an authenticated request's key and the grant it carries.
type caller struct {
	keyID  string // its api_keys id
	policy store.KeyPolicy
}

// may reports whether the caller's grant covers the configured alias name.
func (c caller) may(alias string) bool {
	return c.policy.Aliases == nil || slices.Contains(c.policy.Aliases, alias)
}

// authenticate checks the request's platform API key and finds its grant.
// The key rides x-api-key, as an Anthropic SDK sends it, or a Bearer, as an
// OpenAI SDK does; x-api-key wins when both are present. A repeated header is
// refused as ambiguous, as the control plane refuses it. Every key, the
// bootstrap key included, authenticates by the control plane's rule — an
// active, unexpired row — so a key the platform has archived or let expire
// calls nothing here either; the bootstrap key's value only spares it a
// policy.
func (h *handler) authenticate(r *http.Request) (caller, *apiError) {
	keys := r.Header.Values("x-api-key")
	if len(keys) > 1 || len(r.Header.Values("Authorization")) > 1 {
		return caller{}, unauthenticated("a credential header is repeated")
	}
	key := ""
	if len(keys) == 1 {
		key = keys[0]
	} else if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		key = tok
	}
	if key == "" {
		return caller{}, unauthenticated("x-api-key header is required")
	}
	id, err := apikey.Authenticate(r.Context(), h.cfg.Keys, key)
	if err != nil {
		return caller{}, internal(r, "api key lookup failed", err)
	}
	if id == "" {
		return caller{}, unauthenticated("invalid x-api-key")
	}
	c := caller{keyID: id}
	if p, ok := h.cfg.Catalog.Snapshot().KeyPolicy(id); ok {
		c.policy = p
		return c, nil
	}
	if !h.isBootstrap(key) {
		return caller{}, forbidden("this API key has no model grant; an administrator grants one in the console")
	}
	return c, nil
}

// isBootstrap compares digests, so the comparison is constant-time in the
// key's length as well as its bytes.
func (h *handler) isBootstrap(key string) bool {
	sum := sha256.Sum256([]byte(key))
	return subtle.ConstantTimeCompare(sum[:], h.bootstrap[:]) == 1
}
