// Package clioauth issues and authenticates platform CLI credentials. It uses
// PostgreSQL to share grants between the control plane and model gateway.
package clioauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/identity"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// ClientID is the public OAuth client named by ant's auth login.
	ClientID      = "41077d10-94b8-4194-be48-d251e9eb21b4"
	AccessPrefix  = "sk-map-oat01-"
	refreshPrefix = "sk-map-ort01-"

	Profile   = "user:profile"
	Inference = "user:inference"
	Developer = "user:developer"
	OrgAdmin  = "org:admin"

	// These identifiers describe the installation's reserved tenancy on the
	// CLI wire. The database continues to scope these grants to default.
	OrganizationID = "00000000-0000-4000-8000-000000000001"
	WorkspaceID    = workspace.DefaultPublicID
)

var ErrInvalidGrant = errors.New("cli oauth: invalid grant")

// Authority is the current principal and the grant's consented scopes.
type Authority struct {
	PrincipalID   string
	Role          identity.Role
	WorkspaceRole workspace.Role
	WorkspaceID   string
	WorkspaceKey  string
	Scopes        []string
}

func (a Authority) Has(scope string) bool { return slices.Contains(a.Scopes, scope) }

// CanConsent checks the maximum authority the current role can grant.
func CanConsent(role identity.Role, grant workspace.Role, scopes []string) bool {
	if (!role.AtLeast(identity.RoleUser) && role != identity.RoleBilling) || len(scopes) == 0 {
		return false
	}
	for _, scope := range scopes {
		switch scope {
		case Profile:
		case Inference, Developer:
			if !grant.CanUseResources() {
				return false
			}
		case OrgAdmin:
			if role != identity.RoleAdmin {
				return false
			}
		default:
			return false
		}
	}
	return true
}

type CodeRequest struct {
	ClientID, RedirectURI, State, Challenge string
	Scopes                                  []string
}

type ExchangeRequest struct {
	ClientID, RedirectURI, State, Verifier, Code string
}

type tokenOrganization struct {
	ID   string `json:"uuid"`
	Name string `json:"name"`
}

type tokenAccount struct {
	ID    string `json:"uuid"`
	Email string `json:"email_address"`
}

type tokenWorkspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Tokens has the ten fields observed in the CLI OAuth recording. The token
// endpoint is the only writer of this response.
type Tokens struct {
	AccessToken      string            `json:"access_token"`
	RefreshToken     string            `json:"refresh_token"`
	Type             string            `json:"token_type"`
	ID               string            `json:"token_uuid"`
	Scope            string            `json:"scope"`
	ExpiresIn        int64             `json:"expires_in"`
	RefreshExpiresIn int64             `json:"refresh_token_expires_in"`
	Organization     tokenOrganization `json:"organization"`
	Account          tokenAccount      `json:"account"`
	Workspace        tokenWorkspace    `json:"workspace"`
}

// Prune drops expired grants and their tokens. An authorization prunes before
// taking a principal lock, in the same way browser login prunes its attempts.
func Prune(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `DELETE FROM cli_oauth_grants WHERE expires_at <= statement_timestamp()`)
	return err
}

// IssueCode runs with the authorizing principal and browser session locked by
// the caller, after the consent POST was verified.
func IssueCode(ctx context.Context, tx pgx.Tx, principalID string, in CodeRequest) (string, error) {
	code := secret("")
	_, err := tx.Exec(ctx, `INSERT INTO cli_oauth_grants
		(principal_id, client_id, scopes, code_hash, challenge, state_hash, redirect_uri, expires_at, workspace_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, clock_timestamp()+interval '5 minutes', 'default')`,
		principalID, in.ClientID, in.Scopes, hash(code), in.Challenge, hash(in.State), in.RedirectURI)
	if err != nil {
		return "", err
	}
	return code, nil
}

func Exchange(ctx context.Context, pool *pgxpool.Pool, in ExchangeRequest) (Tokens, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback(ctx)

	// Lock before sampling expiry. A request that waited behind another
	// exchange must see the current code state and database clock.
	var id string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM cli_oauth_grants WHERE code_hash=$1 FOR UPDATE`,
		hash(in.Code)).Scan(&id); err != nil {
		return Tokens{}, grantError(err)
	}
	challenge := sha256.Sum256([]byte(in.Verifier))
	if err := tx.QueryRow(ctx, `UPDATE cli_oauth_grants
		SET code_hash=NULL, challenge='', state_hash='', redirect_uri='',
		    expires_at=clock_timestamp()+interval '30 days'
		WHERE id=$1::uuid AND NOT revoked AND expires_at>clock_timestamp()
		  AND client_id=$2 AND challenge=$3 AND state_hash=$4 AND redirect_uri=$5
		RETURNING id::text`,
		id, in.ClientID, base64.RawURLEncoding.EncodeToString(challenge[:]), hash(in.State), in.RedirectURI).Scan(&id); err != nil {
		return Tokens{}, grantError(err)
	}
	tokens, err := mintTokens(ctx, tx, id)
	if err != nil {
		return Tokens{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tokens{}, err
	}
	return tokens, nil
}

// Refresh serializes rotation on the grant row. The second statement sees
// whether another refresh spent this token while the request waited.
func Refresh(ctx context.Context, pool *pgxpool.Pool, clientID, refreshToken string) (Tokens, error) {
	if !strings.HasPrefix(refreshToken, refreshPrefix) || len(refreshToken) != len(refreshPrefix)+43 {
		return Tokens{}, ErrInvalidGrant
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback(ctx)
	refreshHash := hash(refreshToken)
	var id string
	if err := tx.QueryRow(ctx, `SELECT g.id::text FROM cli_oauth_tokens t JOIN cli_oauth_grants g ON g.id=t.grant_id
		WHERE t.refresh_hash=$1 FOR UPDATE OF g`, refreshHash).Scan(&id); err != nil {
		return Tokens{}, grantError(err)
	}
	var spent bool
	if err := tx.QueryRow(ctx, `SELECT t.spent FROM cli_oauth_tokens t JOIN cli_oauth_grants g ON g.id=t.grant_id
		WHERE t.refresh_hash=$1 AND g.id=$2::uuid AND g.client_id=$3
		  AND NOT g.revoked AND g.expires_at>clock_timestamp()`, refreshHash, id, clientID).Scan(&spent); err != nil {
		return Tokens{}, grantError(err)
	}
	if spent {
		if _, err := tx.Exec(ctx, `UPDATE cli_oauth_grants SET revoked=true WHERE id=$1::uuid`, id); err != nil {
			return Tokens{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Tokens{}, err
		}
		return Tokens{}, ErrInvalidGrant
	}
	if _, err := tx.Exec(ctx, `UPDATE cli_oauth_tokens SET spent=true WHERE refresh_hash=$1`, refreshHash); err != nil {
		return Tokens{}, err
	}
	tokens, err := mintTokens(ctx, tx, id)
	if err != nil {
		return Tokens{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tokens{}, err
	}
	return tokens, nil
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Authenticate looks up an access token with the current principal and grant
// in one database snapshot. Keep the principal attribution for resource writes.
func Authenticate(ctx context.Context, db querier, token string) (Authority, error) {
	if !strings.HasPrefix(token, AccessPrefix) || len(token) != len(AccessPrefix)+43 {
		return Authority{}, ErrInvalidGrant
	}
	var a Authority
	err := db.QueryRow(ctx, `SELECT p.id, p.role, g.scopes, w.public_id, w.id, coalesce(workspace_role(p.id,w.id),'') FROM cli_oauth_tokens t
		JOIN cli_oauth_grants g ON g.id=t.grant_id
		JOIN principals p ON p.id=g.principal_id
		JOIN workspaces w ON w.id=g.workspace_id AND w.org_id=p.org_id AND w.archived_at IS NULL
		WHERE t.access_hash=$1 AND NOT t.spent AND t.access_expires_at>clock_timestamp()
		  AND NOT g.revoked AND g.expires_at>clock_timestamp()
		  AND p.org_id='default' AND p.kind='user' AND p.status='active'`, hash(token)).
		Scan(&a.PrincipalID, &a.Role, &a.Scopes, &a.WorkspaceID, &a.WorkspaceKey, &a.WorkspaceRole)
	return a, grantError(err)
}

func mintTokens(ctx context.Context, tx pgx.Tx, grantID string) (Tokens, error) {
	var role identity.Role
	var grant workspace.Role
	var scopes []string
	out := Tokens{
		AccessToken: secret(AccessPrefix), RefreshToken: secret(refreshPrefix), Type: "Bearer",
		Organization: tokenOrganization{OrganizationID, "Managed Agent Platform"},
	}
	if err := tx.QueryRow(ctx, `SELECT md5(p.id)::uuid::text, p.email, p.role, g.scopes,
		w.public_id, w.name, coalesce(workspace_role(p.id,w.id),'') FROM cli_oauth_grants g
		JOIN principals p ON p.id=g.principal_id
		JOIN workspaces w ON w.id=g.workspace_id AND w.org_id=p.org_id AND w.archived_at IS NULL
		WHERE g.id=$1::uuid AND p.org_id='default' AND p.kind='user' AND p.status='active'`, grantID).
		Scan(&out.Account.ID, &out.Account.Email, &role, &scopes, &out.Workspace.ID, &out.Workspace.Name, &grant); err != nil {
		return Tokens{}, grantError(err)
	}
	if !CanConsent(role, grant, scopes) {
		return Tokens{}, ErrInvalidGrant
	}
	out.Scope = strings.Join(scopes, " ")
	err := tx.QueryRow(ctx, `WITH stamp AS MATERIALIZED (SELECT clock_timestamp() AS at), issued AS (
		INSERT INTO cli_oauth_tokens (grant_id, access_hash, refresh_hash, access_expires_at)
		SELECT g.id, $2, $3, LEAST(stamp.at+interval '8 hours', g.expires_at)
		FROM cli_oauth_grants g CROSS JOIN stamp WHERE g.id=$1::uuid AND g.expires_at>stamp.at
		RETURNING id, access_expires_at)
		SELECT issued.id::text, floor(extract(epoch FROM issued.access_expires_at-stamp.at))::bigint,
		       floor(extract(epoch FROM g.expires_at-stamp.at))::bigint
		FROM issued CROSS JOIN stamp JOIN cli_oauth_grants g ON g.id=$1::uuid`,
		grantID, hash(out.AccessToken), hash(out.RefreshToken)).Scan(&out.ID, &out.ExpiresIn, &out.RefreshExpiresIn)
	if err != nil {
		return Tokens{}, grantError(err)
	}
	return out, nil
}

func grantError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidGrant
	}
	return err
}

func secret(prefix string) string {
	var raw [32]byte
	rand.Read(raw[:])
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:])
}

func hash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
