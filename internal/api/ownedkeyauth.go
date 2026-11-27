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
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
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

// authenticateManagementKey admits only principal-owned public credentials.
// Gateway inference has its own current-owner check.
// Read the owner and credential in one snapshot; this is not an IdP sign-in.
func authenticateManagementKey(ctx context.Context, db apikey.Querier, raw string) (apiKeyAuthority, error) {
	var authority apiKeyAuthority
	var role workspace.Role
	err := db.QueryRow(ctx, `SELECT k.id, p.id, p.kind, coalesce(workspace_role(p.id,k.workspace_id),'')
		FROM api_keys k JOIN principals p ON p.id=k.owner_principal_id
		WHERE k.owned_key_hash=$1 AND k.status='active' AND k.org_id='default' AND k.workspace_id='default'
		  AND (k.expires_at IS NULL OR k.expires_at > clock_timestamp())
		  AND p.org_id='default' AND p.kind IN ('user','service_account') AND p.status='active'`,
		apikey.Hash(raw)).Scan(&authority.KeyID, &authority.OwnerID, &authority.OwnerKind, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return apiKeyAuthority{}, nil
	}
	authority.Role = role.ResourceRole()
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
