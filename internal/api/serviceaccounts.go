package api

import (
	"errors"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/login"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/principal"
)

const (
	consoleServiceAccountsPath       = "/api/console/organizations/{org}/service_accounts"
	consoleServiceAccountPath        = consoleServiceAccountsPath + "/{service_account_id}"
	consoleServiceAccountArchivePath = consoleServiceAccountPath + "/archive"
)

var serviceAccountName = regexp.MustCompile(`^[a-z0-9-]{1,255}$`)

// BetaServiceAccount's adopted native shape. Nullable fields stay present;
// audit IDs are rendered from the referenced row's actual kind, not its name.
type serviceAccountJSON struct {
	ID                string     `json:"id"`
	Type              string     `json:"type"`
	Name              string     `json:"name"`
	Description       *string    `json:"description"`
	OrganizationRole  string     `json:"organization_role"`
	CreatedAt         time.Time  `json:"created_at"`
	CreatedByActorID  *string    `json:"created_by_actor_id"`
	UpdatedAt         time.Time  `json:"updated_at"`
	UpdatedByActorID  *string    `json:"updated_by_actor_id"`
	ArchivedAt        *time.Time `json:"archived_at"`
	ArchivedByActorID *string    `json:"archived_by_actor_id"`
}

type serviceAccountsPageJSON struct {
	Data     []serviceAccountJSON `json:"data"`
	NextPage *string              `json:"next_page"`
}

const serviceAccountColumns = `p.id,p.subject,p.description,p.role,p.created_at,p.created_by,c.kind,
	p.updated_at,p.updated_by,u.kind,p.deactivated_at,p.deactivated_by,a.kind`
const serviceAccountFrom = ` FROM principals p
	LEFT JOIN principals c ON c.id=p.created_by
	LEFT JOIN principals u ON u.id=p.updated_by
	LEFT JOIN principals a ON a.id=p.deactivated_by`

func scanServiceAccount(row pgx.Row) (serviceAccountJSON, error) {
	var a serviceAccountJSON
	var createdKind, updatedKind, archivedKind *string
	err := row.Scan(&a.ID, &a.Name, &a.Description, &a.OrganizationRole, &a.CreatedAt, &a.CreatedByActorID, &createdKind,
		&a.UpdatedAt, &a.UpdatedByActorID, &updatedKind, &a.ArchivedAt, &a.ArchivedByActorID, &archivedKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, errNotFound("Not found")
	}
	if err != nil {
		return a, err
	}
	a.ID, a.Type = principal.PublicID(a.ID, "service_account"), "service_account"
	a.CreatedAt, a.UpdatedAt, a.ArchivedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC(), utcPtr(a.ArchivedAt)
	for _, actor := range []struct {
		id   **string
		kind *string
	}{{&a.CreatedByActorID, createdKind}, {&a.UpdatedByActorID, updatedKind}, {&a.ArchivedByActorID, archivedKind}} {
		if *actor.id != nil && actor.kind != nil {
			alias := principal.PublicID(**actor.id, *actor.kind)
			*actor.id = &alias
		}
	}
	return a, nil
}

func readServiceAccount(r *http.Request, db principal.Querier, id string) (serviceAccountJSON, error) {
	return scanServiceAccount(db.QueryRow(r.Context(), `SELECT `+serviceAccountColumns+serviceAccountFrom+` WHERE p.id=$1 AND p.kind='service_account' AND p.org_id='default'`, id))
}

func (s *server) getServiceAccount(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	id, err := privatePrincipalID(r.PathValue("service_account_id"), "service_account")
	if err != nil {
		return nil, err
	}
	return readServiceAccount(r, s.pool, id)
}

func (s *server) listServiceAccounts(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	q, err := managementQuery(r, "limit", "page", "include_archived")
	if err != nil {
		return nil, err
	}
	p, err := parsePage(q)
	if err != nil {
		return nil, err
	}
	if _, err := managementLimit(q, 100); err != nil {
		return nil, err
	}
	if p.cur != nil && (p.cur.foreignToTime() || p.cur.dir != dirNext || !domain.WellFormedID(p.cur.id, domain.PrefixPrincipal)) {
		return nil, errInvalid("invalid page cursor")
	}
	includeArchived := false
	if q.Has("include_archived") {
		switch q.Get("include_archived") {
		case "true":
			includeArchived = true
		case "false":
		default:
			return nil, errInvalid("include_archived must be a boolean")
		}
	}
	var at *time.Time
	var id string
	if p.cur != nil {
		at, id = &p.cur.t, p.cur.id
	}
	rows, err := s.pool.Query(r.Context(), `SELECT `+serviceAccountColumns+serviceAccountFrom+`
		WHERE p.kind='service_account' AND p.org_id='default' AND ($1 OR p.status='active')
		AND ($2::timestamptz IS NULL OR (p.created_at,p.id)<($2,$3))
		ORDER BY p.created_at DESC,p.id DESC LIMIT $4`, includeArchived, at, id, p.limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := serviceAccountsPageJSON{Data: []serviceAccountJSON{}}
	for rows.Next() {
		a, err := scanServiceAccount(rows)
		if err != nil {
			return nil, err
		}
		page.Data = append(page.Data, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Data) > p.limit {
		page.Data = page.Data[:p.limit]
		last := page.Data[len(page.Data)-1]
		internalID, _, _ := principal.ParsePublicID(last.ID)
		next := encodeTimeCursor(dirNext, last.CreatedAt, internalID)
		page.NextPage = &next
	}
	return page, nil
}

func (s *server) createServiceAccount(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	obj, err := managementBody(r, "name", "description", "organization_role")
	if err != nil {
		return nil, err
	}
	name, err := requiredString(obj, "name")
	if err != nil {
		return nil, err
	}
	if !serviceAccountName.MatchString(name) {
		return nil, errInvalid("name must be 1–255 lowercase letters, digits or hyphens")
	}
	description, _, _, err := stringField(obj, "description")
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(description) > 2000 {
		return nil, errInvalid("description must not exceed 2000 characters")
	}
	role, set, null, err := stringField(obj, "organization_role")
	if err != nil {
		return nil, err
	}
	if !set {
		role = "developer"
	} else if null || !managementRole(role, false) {
		return nil, errInvalid("unsupported organization_role")
	}
	tx, actor, err := s.beginPrincipalMutation(r)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(r.Context())
	id := domain.NewID(domain.PrefixPrincipal).String()
	_, err = tx.Exec(r.Context(), `INSERT INTO principals(id,issuer,subject,kind,role,description,created_by)
		VALUES ($1,$2,$3,'service_account',$4,$5,$6)`, id, login.ServiceAccountIssuer, name, role, description, actor.PrincipalID)
	if err := principalNameConflict(err); err != nil {
		return nil, err
	}
	a, err := readServiceAccount(r, tx, id)
	if err != nil {
		return nil, err
	}
	return a, tx.Commit(r.Context())
}

func (s *server) updateServiceAccount(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	id, err := privatePrincipalID(r.PathValue("service_account_id"), "service_account")
	if err != nil {
		return nil, err
	}
	obj, err := managementBody(r, "description", "organization_role")
	if err != nil {
		return nil, err
	}
	description, descriptionSet, _, err := stringField(obj, "description")
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(description) > 2000 {
		return nil, errInvalid("description must not exceed 2000 characters")
	}
	role, roleSet, roleNull, err := stringField(obj, "organization_role")
	if err != nil {
		return nil, err
	}
	roleSet = roleSet && !roleNull
	if roleSet && !managementRole(role, false) {
		return nil, errInvalid("unsupported organization_role")
	}
	tx, actor, err := s.beginPrincipalMutation(r)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(r.Context())
	// Test stored status, not only archived_at: SQL recovery may deactivate a
	// row without API metadata. No update may resurrect it.
	var status string
	err = tx.QueryRow(r.Context(), `SELECT status FROM principals WHERE id=$1 AND kind='service_account' AND org_id='default'`, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("Not found")
	}
	if err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, errInvalid("archived service accounts cannot be updated")
	}
	if _, err := tx.Exec(r.Context(), `UPDATE principals SET description=CASE WHEN $2 THEN $3 ELSE description END,
		role=CASE WHEN $4 THEN $5 ELSE role END,updated_at=clock_timestamp(),updated_by=$6 WHERE id=$1`, id, descriptionSet, description, roleSet, role, actor.PrincipalID); err != nil {
		return nil, err
	}
	a, err := readServiceAccount(r, tx, id)
	if err != nil {
		return nil, err
	}
	return a, tx.Commit(r.Context())
}

func (s *server) archiveServiceAccount(r *http.Request) (any, error) {
	if _, err := requireHumanAdmin(r); err != nil {
		return nil, err
	}
	id, err := privatePrincipalID(r.PathValue("service_account_id"), "service_account")
	if err != nil {
		return nil, err
	}
	tx, actor, err := s.beginPrincipalMutation(r)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(r.Context())
	a, err := readServiceAccount(r, tx, id)
	if err != nil {
		return nil, err
	}
	if a.ArchivedAt == nil {
		if _, err := tx.Exec(r.Context(), `UPDATE principals SET status='deactivated',deactivated_at=clock_timestamp(),deactivated_by=$2,updated_at=clock_timestamp(),updated_by=$2 WHERE id=$1`, id, actor.PrincipalID); err != nil {
			return nil, err
		}
		a, err = readServiceAccount(r, tx, id)
		if err != nil {
			return nil, err
		}
	}
	return a, tx.Commit(r.Context())
}
