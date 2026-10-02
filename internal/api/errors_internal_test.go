package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
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

// TestDetailsKeepTheStatusVisible: a detailed error is still an *apiError to
// errors.As, so the callers that classify an error by its status — a session
// resource's outcome, a gone session, a skill read — see it through the
// details as they did before #664.
func TestDetailsKeepTheStatusVisible(t *testing.T) {
	var ae *apiError
	err := withDetails(errNotFound("environment %s not found", "env_1"), errorDetails{ErrorVisibility: visibilityUserFacing})
	if !errors.As(err, &ae) || ae.status != http.StatusNotFound {
		t.Fatalf("errors.As through withDetails = %v (%+v), want the 404 underneath", errors.As(err, &ae), ae)
	}
}

// TestABareHeartbeatMismatchIsStillA412: the queue's refusals carry the item
// now, but the sentinel alone keeps the 412 it always had, without details,
// rather than falling through to an internal fault.
func TestABareHeartbeatMismatchIsStillA412(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/x", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID, "req_x"))
	w := httptest.NewRecorder()
	writeError(w, r, mapWorkErr(queue.ErrHeartbeatMismatch))
	want := `{"error":{"message":"expected_last_heartbeat does not match the current lease","type":"invalid_request_error"},"request_id":"req_x","type":"error"}` + "\n"
	if w.Code != http.StatusPreconditionFailed || w.Body.String() != want {
		t.Errorf("bare sentinel = %d %s, want 412 %s", w.Code, w.Body.String(), want)
	}
}

// TestAHeartbeatMismatchRendersTheLastBeatAsTheReferenceDoes pins the 412
// sentence's two timestamp shapes: six fractional digits, as recorded
// (2026-09-02 batch2 idx 259 `work.heartbeat.NO_HEARTBEAT`), and none on a
// whole second, where Python's isoformat, which the reference renders with,
// drops an all-zero fraction (INFERRED, unrecorded).
func TestAHeartbeatMismatchRendersTheLastBeatAsTheReferenceDoes(t *testing.T) {
	for _, tc := range []struct {
		beat time.Time
		want string
	}{
		{time.Date(2026, 9, 2, 0, 8, 33, 477978000, time.UTC), "2026-09-02T00:08:33.477978Z"},
		{time.Date(2026, 9, 2, 0, 8, 33, 120000000, time.UTC), "2026-09-02T00:08:33.120000Z"},
		{time.Date(2026, 9, 2, 0, 8, 33, 0, time.UTC), "2026-09-02T00:08:33Z"},
		{time.Date(2026, 9, 2, 2, 8, 33, 0, time.FixedZone("x", 2*3600)), "2026-09-02T00:08:33Z"},
	} {
		beat := tc.beat
		err := mapWorkErr(&queue.HeartbeatMismatchError{
			Item: &queue.Work{State: "active", LastHeartbeat: &beat}, TTLSeconds: 30, Expected: "NO_HEARTBEAT"})
		var ae *apiError
		if !errors.As(err, &ae) {
			t.Fatalf("%v: %v, want an API error", tc.beat, err)
		}
		if want := "Heartbeat precondition failed: expected NO_HEARTBEAT, actual was " + tc.want; ae.message != want {
			t.Errorf("%v: %q, want %q", tc.beat, ae.message, want)
		}
	}
}

// TestCapForLogCutsAtARuneBoundary pins the refusal line's cap: a short string
// passes whole, a long one is cut to at most logFieldMax bytes without
// splitting a rune, and the cut is marked.
func TestCapForLogCutsAtARuneBoundary(t *testing.T) {
	if got := capForLog("short"); got != "short" {
		t.Errorf("short: %q", got)
	}
	exact := strings.Repeat("a", logFieldMax)
	if got := capForLog(exact); got != exact {
		t.Errorf("exactly the cap was cut: %d bytes", len(got))
	}
	// 'a' then two-byte runes: byte logFieldMax falls inside a rune.
	got := capForLog("a" + strings.Repeat("é", logFieldMax))
	body, ok := strings.CutSuffix(got, "…[truncated]")
	if !ok || !utf8.ValidString(body) || len(body) != logFieldMax-1 {
		t.Errorf("cut = %d bytes, valid %v, marked %v; want %d valid bytes, marked",
			len(body), utf8.ValidString(body), ok, logFieldMax-1)
	}
}
