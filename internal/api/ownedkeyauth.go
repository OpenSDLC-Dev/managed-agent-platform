package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/apikey"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/jackc/pgx/v5"
)

// apiKeyAuthority keeps the authenticated credential separate from its current
// owner authority. Audit and Memory actors name KeyID, never OwnerID.
type apiKeyAuthority struct {
	KeyID, OwnerID, OwnerKind string
	Role                      identity.Role
}

type ownedKeyContextKey struct{}

func ownedKeyFrom(ctx context.Context) (apiKeyAuthority, bool) {
	authority, ok := ctx.Value(ownedKeyContextKey{}).(apiKeyAuthority)
	return authority, ok
}

// authenticateManagementKey is control-plane-only. The shared apikey lookup
// deliberately cannot admit owned digests to the gateway or private key API.
// Read the owner and credential in one snapshot; this is not an IdP sign-in.
func authenticateManagementKey(ctx context.Context, db apikey.Querier, raw string) (apiKeyAuthority, error) {
	var authority apiKeyAuthority
	err := db.QueryRow(ctx, `SELECT k.id, coalesce(k.owner_principal_id,''),
		coalesce(p.kind,''), coalesce(p.role,'')
		FROM api_keys k LEFT JOIN principals p ON p.id=k.owner_principal_id
		WHERE coalesce(k.key_hash,k.owned_key_hash)=$1 AND k.status='active'
		  AND (k.expires_at IS NULL OR k.expires_at > clock_timestamp())
		  AND (k.owner_principal_id IS NULL OR
		       (p.org_id='default' AND p.kind IN ('user','service_account') AND p.status='active'))`,
		apikey.Hash(raw)).Scan(&authority.KeyID, &authority.OwnerID, &authority.OwnerKind, &authority.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return apiKeyAuthority{}, nil
	}
	return authority, err
}

// publicAPIPaths classifies both sides of a possible ServeMux cleaning redirect.
// Human sign-in requires neither side to be public; owned admission requires
// both. Otherwise /api/../v1 could provision before redirecting, or /v1/../api
// could carry owned authority toward a private route.
func publicAPIPaths(r *http.Request) (requested, routed bool) {
	p := r.URL.EscapedPath()
	requested = publicAPISegment(p)
	// ServeMux cleans escaped paths before unescaping individual segments, except
	// on CONNECT. Encoded dots/slashes are literals, not cleaning instructions.
	// Its trailing-slash restoration cannot change the first segment.
	if r.Method != http.MethodConnect {
		p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	}
	return requested, publicAPISegment(p)
}

func publicAPISegment(p string) bool {
	first, _, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	value, err := url.PathUnescape(first)
	return err == nil && value == "v1"
}

func issuedAPIKeyBearer(r *http.Request) bool {
	token, ok := bearerToken(r)
	return ok && strings.HasPrefix(token, IssuedKeyPrefix)
}
