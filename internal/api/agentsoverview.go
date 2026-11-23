package api

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

const consoleAgentsOverviewPath = consoleWorkspacePath + "/agents/overview"

//go:embed agentsoverview.sql
var agentsOverviewSQL string

func (s *server) agentsOverview(r *http.Request) (any, error) {
	if _, err := requireCookieSession(r); err != nil {
		return nil, err
	}
	if err := consoleWorkspace(r); err != nil {
		return nil, err
	}
	q, err := managementQuery(r, "agent_id", "sort", "direction")
	if err != nil {
		return nil, err
	}
	sort := q.Get("sort")
	if sort == "" {
		sort = "sessions"
	}
	switch sort {
	case "name", "sessions", "error_rate", "last_active":
	default:
		return nil, errInvalid("unsupported overview sort")
	}
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	if direction != "asc" && direction != "desc" {
		return nil, errInvalid("direction must be asc or desc")
	}
	id := q.Get("agent_id")
	if q.Has("agent_id") {
		if err := checkAgentPathID(id); err != nil {
			return nil, err
		}
	}
	// Only the allowlisted ORDER BY terms enter SQL. All scope and resource
	// identifiers remain bind parameters. Charts and totals share one MVCC snapshot.
	if sort == "error_rate" {
		sort = "CASE WHEN sessions > 0 THEN error_rate END"
	}
	order := sort + " " + direction + " NULLS LAST, name ASC, id ASC"
	var report json.RawMessage
	err = s.pool.QueryRow(r.Context(), strings.Replace(agentsOverviewSQL, "/* table order */", order, 1), r.PathValue("org"), r.PathValue("workspace"), id).Scan(&report)
	return report, err
}
