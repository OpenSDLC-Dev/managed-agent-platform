package api

import (
	"context"
	"encoding/json"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
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

// orEmptyRaw keeps a nil list from reaching a jsonb column as `null`, where it
// would read back as a nil slice and render as null on a required array field.
func orEmptyRaw(rs []json.RawMessage) []json.RawMessage {
	if rs == nil {
		return []json.RawMessage{}
	}
	return rs
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
// at every fire. A file rubric needs object storage to snapshot into, and a
// deployment configured without it would record a failed run every night
// instead of being refused once — so the check runs here, with the message the
// event path already uses, rather than being deferred to the fire.
//
// The per-type field validation is not repeated: a fire runs the same
// NormalizeInbound a posted batch gets, and duplicating it here would be a
// second, drifting copy of the same rules.
func (s *server) validateDeploymentInitialEvents(initial []json.RawMessage) error {
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
