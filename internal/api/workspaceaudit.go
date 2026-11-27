package api

import (
	"net/http"
	"slices"
	"time"
)

type auditWorkspace struct {
	ID            string            `json:"id"`
	Type          string            `json:"type"`
	Name          string            `json:"name"`
	DisplayColor  string            `json:"display_color"`
	CreatedAt     time.Time         `json:"created_at"`
	ArchivedAt    *time.Time        `json:"archived_at"`
	Tags          map[string]string `json:"tags"`
	CompartmentID *string           `json:"compartment_id"`
	ExternalKeyID *string           `json:"external_key_id"`
	DataResidency any               `json:"data_residency"`
}

func (s *server) cliWorkspaces(r *http.Request) (any, error) {
	q, err := managementQuery(r, "after_id", "before_id", "limit", "include_default", "include_archived")
	if err != nil {
		return nil, err
	}
	limit, err := managementLimit(q, 1000)
	if err != nil {
		return nil, err
	}
	archived, err := parseBoolParam(q, "include_archived")
	if err != nil {
		return nil, err
	}
	includeDefault, err := parseBoolParam(q, "include_default")
	if err != nil {
		return nil, err
	}
	ids := append(slices.Clone(q["ids[]"]), q["ids"]...)
	if len(ids) > 100 {
		return nil, errInvalid("ids accepts at most 100 workspace IDs")
	}
	for _, id := range ids {
		if !storableText(id) {
			return nil, errInvalid("invalid workspace ID")
		}
	}
	if q.Has("after_id") && q.Has("before_id") {
		return nil, errInvalid("after_id and before_id are mutually exclusive")
	}
	stmt := `WITH eligible AS (
	SELECT public_id,name,created_at,archived_at FROM workspaces
	WHERE org_id='default' AND ($1 OR id<>'default') AND ($2 OR archived_at IS NULL)
	AND ($3::text[] IS NULL OR public_id=ANY($3))
	) SELECT public_id,name,created_at,archived_at FROM eligible`
	args := []any{includeDefault, archived, ids, limit + 1}
	order := "DESC"
	if q.Has("after_id") || q.Has("before_id") {
		comp, cursor := "<", q.Get("after_id")
		if q.Has("before_id") {
			comp, cursor, order = ">", q.Get("before_id"), "ASC"
		}
		stmt += " WHERE (created_at,public_id) " + comp + " (SELECT created_at,public_id FROM eligible WHERE public_id=$5)"
		args = append(args, cursor)
	}
	rows, err := s.pool.Query(r.Context(), stmt+" ORDER BY created_at "+order+", public_id "+order+" LIMIT $4", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := struct {
		Data    []auditWorkspace `json:"data"`
		HasMore bool             `json:"has_more"`
		FirstID *string          `json:"first_id"`
		LastID  *string          `json:"last_id"`
	}{Data: []auditWorkspace{}}
	for rows.Next() {
		w := auditWorkspace{Type: "workspace", DisplayColor: "#6C5BB9", Tags: map[string]string{}}
		if err := rows.Scan(&w.ID, &w.Name, &w.CreatedAt, &w.ArchivedAt); err != nil {
			return nil, err
		}
		out.Data = append(out.Data, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Data) > limit {
		out.HasMore = true
		out.Data = out.Data[:limit]
	}
	if order == "ASC" {
		slices.Reverse(out.Data)
	}
	if len(out.Data) > 0 {
		out.FirstID = &out.Data[0].ID
		out.LastID = &out.Data[len(out.Data)-1].ID
	}
	return out, nil
}
