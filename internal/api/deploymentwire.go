package api

import (
	"context"
	"encoding/json"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/events"
)

// nullableString reads a field whose wire type is string-or-null, which
// stringField cannot express: it collapses an explicit null onto the empty
// string, and a deployment's description needs the two apart — null is unset,
// "" is a description the caller chose.
func nullableString(obj map[string]json.RawMessage, key string) (val *string, set, null bool, err error) {
	raw, ok := obj[key]
	if !ok {
		return nil, false, false, nil
	}
	if isNull(raw) {
		return nil, true, true, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, true, false, errInvalid("%s must be a string or null", key)
	}
	return &s, true, false, nil
}

// nullableText maps the empty string onto a NULL column. The schedule pair is
// stored as two nullable columns under a CHECK that they are both null or both
// set, so "no schedule" has to reach the database as NULL rather than as an empty string.
func nullableText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

// principalPtr is the audit-only created_by: the API key or principal that
// created the row, or nil when the caller is unattributed. createSession's
// rule, and it matters more on a deployment — a session a schedule fires has
// no creator at all, so the deployment row is where the trail survives.
func principalPtr(ctx context.Context) *string {
	if p := principalFrom(ctx); p != "" {
		return &p
	}
	return nil
}

// validateDeploymentInitialEvents refuses at create what would otherwise fail
// at every fire — twice over, because there are two ways a stored list can be
// unfirable.
//
// The first is the list itself. A fire runs events.NormalizeInbound, the same
// normalizer a posted batch gets, so a list that cannot normalize records a
// failed run every night forever. That normalizer is called here rather than
// reimplemented: its rules stay in one place, and its adjacency rule — a
// system.message last, and immediately after a user.message — makes this
// platform narrower than the union the reference publishes, which is plan 37
// §8.1 entry 23. The environment kind it takes is not threaded through:
// user.tool_result is the only type that kind gates, and parseInitialEvents
// has already refused every type but the three a deployment admits.
//
// The second is a file rubric with no object storage to snapshot into. That
// one the normalizer cannot see, so it is checked here with the message the
// event path already uses.
func (s *server) validateDeploymentInitialEvents(initial []json.RawMessage) error {
	if _, err := events.NormalizeInbound("", initial); err != nil {
		return errInvalid("initial_events: %s", err)
	}
	if s.blobs != nil {
		return nil
	}
	for _, raw := range initial {
		var ev struct {
			Type   string `json:"type"`
			Rubric struct {
				Type string `json:"type"`
			} `json:"rubric"`
		}
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue // NormalizeInbound's to judge, at the fire.
		}
		if ev.Type == string(domain.EventUserDefineOutcome) && ev.Rubric.Type == "file" {
			return errInvalid("file rubrics require the files surface, which this deployment does not configure")
		}
	}
	return nil
}
