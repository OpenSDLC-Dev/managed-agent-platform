package api

import (
	"errors"
	"net/http"
	"slices"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/clioauth"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
	"github.com/jackc/pgx/v5"
)

const adminAPIKeysPath = "/v1/organizations/api_keys"

// As checked against anthropic-sdk-go v1.70.1 — betaorganizationapikey.go BetaAPIKey.
// The public resource has workspace scope and a nullable typed creator and hint.
type adminAPIKeyJSON struct {
	apiKeyMetadataJSON
	CreatedBy      *actorJSON `json:"created_by"`
	PartialKeyHint *string    `json:"partial_key_hint"`
	Scope          struct {
		Type        string `json:"type"`
		WorkspaceID string `json:"workspace_id"`
	} `json:"scope"`
}

func renderAdminAPIKey(k ManagementKey) adminAPIKeyJSON {
	out := adminAPIKeyJSON{apiKeyMetadataJSON: apiKeyMetadata(k)}
	out.Scope.Type, out.Scope.WorkspaceID = "workspace", clioauth.WorkspaceID
	if k.PartialKeyHint != "" {
		out.PartialKeyHint = &k.PartialKeyHint
	}
	// Human issuance recorded a principal ID even before keys had owners.
	// A legacy machine-key issuer carries no public user or service-account ID.
	if k.CreatedBy != nil {
		if id := principal.PublicID(*k.CreatedBy, "user"); id != "" {
			out.CreatedBy = &actorJSON{ID: id, Type: "user"}
		}
	}
	return out
}

const servedAPIKeyTenancy = `org_id='default' AND workspace_id='default'`

func (s *server) getAdminAPIKey(r *http.Request) (any, error) {
	id := r.PathValue("id")
	if !consoleIDShape(id, domain.PrefixAPIKey) {
		return nil, errNotFound("Not found")
	}
	k, err := scanManagementKey(s.pool.QueryRow(r.Context(), `SELECT `+managementKeyColumns+` FROM api_keys WHERE `+servedAPIKeyTenancy+` AND id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("Not found")
	}
	if err != nil {
		return nil, err
	}
	return renderAdminAPIKey(k), nil
}

type adminAPIKeysPage struct {
	Data    []adminAPIKeyJSON `json:"data"`
	FirstID *string           `json:"first_id"`
	HasMore bool              `json:"has_more"`
	LastID  *string           `json:"last_id"`
}

func (s *server) listAdminAPIKeys(r *http.Request) (any, error) {
	q, err := managementQuery(r, "limit", "after_id", "before_id", "status", "workspace_id", "created_by_user_id")
	if err != nil {
		return nil, err
	}
	limit, err := managementLimit(q, 1000)
	if err != nil {
		return nil, err
	}
	after, before := q.Get("after_id"), q.Get("before_id")
	if after != "" && before != "" {
		return nil, errInvalid("after_id and before_id are mutually exclusive")
	}
	status := q.Get("status")
	if q.Has("status") {
		switch status {
		case KeyStatusActive, KeyStatusArchived, KeyStatusExpired, KeyStatusInactive:
		default:
			return nil, errInvalid("invalid API key status")
		}
	}
	page := adminAPIKeysPage{Data: []adminAPIKeyJSON{}}
	if q.Has("workspace_id") && q.Get("workspace_id") != clioauth.WorkspaceID {
		return page, nil
	}
	creator := ""
	if q.Has("created_by_user_id") {
		id, kind, ok := principal.ParsePublicID(q.Get("created_by_user_id"))
		if !ok || kind != "user" {
			return page, nil
		}
		creator = id
	}

	// Compare and return the same projected status, including expiry. A cursor
	// is a position in this tenancy even if its row does not match the filters.
	query := `SELECT * FROM (SELECT ` + managementKeyColumns + ` FROM api_keys WHERE ` + servedAPIKeyTenancy +
		` AND ($2::text='' OR created_by=$2)`
	order := "DESC"
	args := []any{status, creator, limit + 1}
	if after != "" || before != "" {
		cursor, comparison := after, "<"
		if before != "" {
			cursor, comparison, order = before, ">", "ASC"
		}
		query += ` AND (created_at,id) ` + comparison + ` (SELECT created_at,id FROM api_keys WHERE ` + servedAPIKeyTenancy + ` AND id=$4)`
		args = append(args, cursor)
	}
	query += `) AS projected WHERE ($1::text='' OR status=$1) ORDER BY created_at ` + order + `, id ` + order + ` LIMIT $3`
	rows, err := s.pool.Query(r.Context(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		k, err := scanManagementKey(rows)
		if err != nil {
			return nil, err
		}
		page.Data = append(page.Data, renderAdminAPIKey(k))
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Data) > limit {
		page.HasMore = true
		page.Data = page.Data[:limit]
	}
	if before != "" {
		slices.Reverse(page.Data)
	}
	if len(page.Data) > 0 {
		page.FirstID = &page.Data[0].ID
		page.LastID = &page.Data[len(page.Data)-1].ID
	}
	return page, nil
}
