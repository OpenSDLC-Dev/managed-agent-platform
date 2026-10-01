package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnErrorWithoutDetailsRendersAsBefore pins writeError's bytes for errors
// that carry no details — a plain one, the memory path conflict whose
// schema-declared members stay flat inside `error`, and the header-carrying
// target-store hold — so the seam that nests `details` (#664) cannot move them.
func TestAnErrorWithoutDetailsRendersAsBefore(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"plain": {errInvalid("bad %s", "field"),
			`{"error":{"message":"bad field","type":"invalid_request_error"},"request_id":"req_x","type":"error"}`},
		"memory path conflict": {errMemoryPathConflict("mem_1", "/a", "path %s is taken", "/a"),
			`{"error":{"conflicting_memory_id":"mem_1","conflicting_path":"/a","message":"path /a is taken","type":"memory_path_conflict_error"},"request_id":"req_x","type":"error"}`},
		"target store held": {errTargetStoreHeld("store %s is held", "memstore_1"),
			`{"error":{"message":"store memstore_1 is held","type":"conflict_error"},"request_id":"req_x","type":"error"}`},
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
			r = r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, "req_x"))
			w := httptest.NewRecorder()
			writeError(w, r, tc.err)
			if got := w.Body.String(); got != tc.want+"\n" {
				t.Errorf("rendered\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

// TestDetailsNestInsideTheError pins where `details` renders: a member of
// `error`, its own members in the order every recorded body lists them
// (2026-09-02 batch2 `work.heartbeat.NO_HEARTBEAT`, 2026-09-05 batch2
// `rec83.edge3.issue.unknown-env`). The members around it keep the sorted
// order the whole envelope has always had.
func TestDetailsNestInsideTheError(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, "req_x"))
	w := httptest.NewRecorder()
	writeError(w, r, withDetails(errNotFound("environment %s not found", "env_1"),
		errorDetails{CurrentState: map[string]any{"state": "active"}, ErrorVisibility: visibilityUserFacing, ErrorCode: "environment_not_found"}))
	want := `{"error":{"details":{"current_state":{"state":"active"},"error_visibility":"user_facing","error_code":"environment_not_found"},` +
		`"message":"environment env_1 not found","type":"not_found_error"},"request_id":"req_x","type":"error"}` + "\n"
	if got := w.Body.String(); got != want {
		t.Errorf("rendered\n  %s\nwant\n  %s", got, want)
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
