package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The dream wire surface (plan 41, #475): shapes per the pinned SDK's
// BetaDream, bounds per the OpenAPI spec the SDK is generated from. The
// create's last rule is `update_existing`'s (slice 4): the target must be the
// dream's own memory_store input, and at most one live in-place dream may hold
// a store — a second is the 409 BetaTargetStoreHeldError names.

// dreamFields is BetaDream's field set (anthropic-sdk-go v1.70.1
// betadream.go:150-178): all fourteen are api:"required" and the spec forbids
// extra keys, so the resource renders exactly these, always.
var dreamFields = []string{
	"type", "id", "status", "inputs", "outputs", "model", "instructions",
	"output_behavior", "session_id", "created_at", "ended_at", "archived_at",
	"usage", "error",
}

// createDreamInputs returns an unarchived memory store and n live session ids —
// the one input pair every create needs.
func createDreamInputs(t *testing.T, s *tserver, n int) (storeID string, sessionIDs []any) {
	t.Helper()
	storeID = createMemoryStore(t, s, "dream-input")
	agentID, envID := fixture(t, s)
	for range n {
		sessionIDs = append(sessionIDs, createSession(t, s,
			map[string]any{"agent": agentID, "environment_id": envID})["id"])
	}
	return storeID, sessionIDs
}

func dreamInputs(storeID string, sessionIDs []any) []any {
	return []any{
		map[string]any{"type": "memory_store", "memory_store_id": storeID},
		map[string]any{"type": "sessions", "session_ids": sessionIDs},
	}
}

func dreamBody(storeID string, sessionIDs []any) map[string]any {
	return map[string]any{"inputs": dreamInputs(storeID, sessionIDs), "model": "claude-opus-4-8"}
}

func createDream(t *testing.T, s *tserver, body map[string]any) map[string]any {
	t.Helper()
	status, res := s.do(http.MethodPost, "/v1/dreams", body)
	if status != http.StatusOK {
		t.Fatalf("create dream: status %d (%v)", status, res)
	}
	return res
}

// The golden test: the resource is exactly BetaDream's fourteen keys, with the
// values slice 1 can produce.
func TestDreamCreate(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 2)

	body := createDream(t, s, dreamBody(storeID, sessionIDs))
	if len(body) != len(dreamFields) {
		t.Errorf("dream renders %d keys, want %d: %v", len(body), len(dreamFields), body)
	}
	wantOnlyFields(t, body, dreamFields...)

	id, _ := body["id"].(string)
	if !strings.HasPrefix(id, "drm_") {
		t.Fatalf("id %q lacks the drm_ prefix", id)
	}
	if body["type"] != "dream" || body["status"] != "pending" {
		t.Errorf("type/status = %v/%v, want dream/pending", body["type"], body["status"])
	}
	if outs, ok := body["outputs"].([]any); !ok || len(outs) != 0 {
		t.Errorf("outputs = %v, want an empty array", body["outputs"])
	}
	for _, k := range []string{"session_id", "ended_at", "archived_at", "error", "instructions"} {
		if body[k] != nil {
			t.Errorf("%s = %v, want null", k, body[k])
		}
	}
	// model renders through domain.Model: no speed key when none was sent.
	model, _ := body["model"].(map[string]any)
	if len(model) != 1 || model["id"] != "claude-opus-4-8" {
		t.Errorf("model = %v, want {id: claude-opus-4-8} alone", body["model"])
	}
	if ob, _ := body["output_behavior"].(map[string]any); len(ob) != 1 || ob["type"] != "create_new" {
		t.Errorf("output_behavior = %v, want {type: create_new}", body["output_behavior"])
	}
	usage, _ := body["usage"].(map[string]any)
	if len(usage) != 4 {
		t.Fatalf("usage = %v, want the four flat counters", body["usage"])
	}
	for _, k := range []string{"input_tokens", "output_tokens",
		"cache_read_input_tokens", "cache_creation_input_tokens"} {
		if v, ok := usage[k].(float64); !ok || v != 0 {
			t.Errorf("usage.%s = %v, want 0", k, usage[k])
		}
	}
	// inputs are echoed as sent, in order.
	inputs, _ := body["inputs"].([]any)
	if len(inputs) != 2 {
		t.Fatalf("inputs = %v, want the two sent", body["inputs"])
	}
	first, _ := inputs[0].(map[string]any)
	second, _ := inputs[1].(map[string]any)
	if first["type"] != "memory_store" || first["memory_store_id"] != storeID {
		t.Errorf("inputs[0] = %v, want the memory_store input", inputs[0])
	}
	got, _ := second["session_ids"].([]any)
	if second["type"] != "sessions" || len(got) != 2 || got[0] != sessionIDs[0] || got[1] != sessionIDs[1] {
		t.Errorf("inputs[1] = %v, want the sessions input in order", inputs[1])
	}
	stamp(t, body["created_at"]) // RFC 3339, and parseable
}

// The object model form, instructions, and an explicit create_new all echo.
func TestDreamCreateEchoesObjectModel(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)

	req := dreamBody(storeID, sessionIDs)
	req["model"] = map[string]any{"id": "claude-opus-4-8", "speed": "fast"}
	req["instructions"] = "merge the duplicates"
	req["output_behavior"] = map[string]any{"type": "create_new"}
	body := createDream(t, s, req)

	model, _ := body["model"].(map[string]any)
	if len(model) != 2 || model["id"] != "claude-opus-4-8" || model["speed"] != "fast" {
		t.Errorf("model = %v, want {id, speed: fast}", body["model"])
	}
	if body["instructions"] != "merge the duplicates" {
		t.Errorf("instructions = %v, want the string sent", body["instructions"])
	}
	if ob, _ := body["output_behavior"].(map[string]any); len(ob) != 1 || ob["type"] != "create_new" {
		t.Errorf("output_behavior = %v, want {type: create_new}", body["output_behavior"])
	}
	// The stored resource reads back identically.
	status, got := s.do(http.MethodGet, "/v1/dreams/"+body["id"].(string), nil)
	if status != http.StatusOK {
		t.Fatalf("get: status %d (%v)", status, got)
	}
	if fmt.Sprint(got) != fmt.Sprint(body) {
		t.Errorf("get body = %v, want the create body %v", got, body)
	}
}

func TestDreamCreateRejections(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)
	sesn := sessionIDs[0].(string)
	otherStore := createMemoryStore(t, s, "dream-other")
	archivedStore := createMemoryStore(t, s, "dream-archived")
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+archivedStore+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive store: status %d (%v)", status, body)
	}
	// A well-formed id no row carries.
	const ghostStore = "memstore_0123456789abcdefghjkmnpq"
	const ghostSession = "sesn_0123456789abcdefghjkmnpq"

	many := make([]any, 101)
	for i := range many {
		many[i] = ghostSession
	}

	model := "claude-opus-4-8"
	with := func(inputs []any) map[string]any {
		return map[string]any{"inputs": inputs, "model": model}
	}
	alias := "session_" + strings.TrimPrefix(sesn, "sesn_")
	storeInput := map[string]any{"type": "memory_store", "memory_store_id": storeID}
	sessionsInput := map[string]any{"type": "sessions", "session_ids": []any{sesn}}

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"no inputs", map[string]any{"model": model}},
		{"inputs not an array", map[string]any{"inputs": "both", "model": model}},
		{"element without type", with([]any{map[string]any{"memory_store_id": storeID}, sessionsInput})},
		{"unknown element type", with([]any{map[string]any{"type": "vault"}, sessionsInput})},
		{"two memory_store inputs", with([]any{storeInput, storeInput, sessionsInput})},
		{"two sessions inputs", with([]any{storeInput, sessionsInput, sessionsInput})},
		{"no sessions input", with([]any{storeInput})},
		{"memory_store arm carrying session_ids", with([]any{
			map[string]any{"type": "memory_store", "memory_store_id": storeID, "session_ids": []any{sesn}},
			sessionsInput})},
		{"sessions arm carrying memory_store_id", with([]any{storeInput,
			map[string]any{"type": "sessions", "session_ids": []any{sesn}, "memory_store_id": storeID}})},
		{"zero session ids", with([]any{storeInput,
			map[string]any{"type": "sessions", "session_ids": []any{}}})},
		{"unknown memory store", with([]any{
			map[string]any{"type": "memory_store", "memory_store_id": ghostStore}, sessionsInput})},
		{"archived memory store", with([]any{
			map[string]any{"type": "memory_store", "memory_store_id": archivedStore}, sessionsInput})},
		{"unknown session", with([]any{storeInput,
			map[string]any{"type": "sessions", "session_ids": []any{ghostSession}}})},
		{"no model", map[string]any{"inputs": dreamInputs(storeID, sessionIDs)}},
		{"empty model", dreamWithModel(storeID, sessionIDs, "")},
		{"empty model object", dreamWithModel(storeID, sessionIDs, map[string]any{})},
		{"unknown model speed", dreamWithModel(storeID, sessionIDs,
			map[string]any{"id": model, "speed": "turbo"})},
		{"unknown key in the model object", dreamWithModel(storeID, sessionIDs,
			map[string]any{"id": model, "extra": 1})},
		{"empty instructions", dreamWith(storeID, sessionIDs, "instructions", "")},
		{"over-long instructions", dreamWith(storeID, sessionIDs, "instructions", strings.Repeat("x", 4097))},
		{"unknown output_behavior type", dreamWith(storeID, sessionIDs, "output_behavior",
			map[string]any{"type": "other"})},
		{"create_new carrying a target", dreamWith(storeID, sessionIDs, "output_behavior",
			map[string]any{"type": "create_new", "memory_store_id": storeID})},
		{"unknown top-level key", func() map[string]any {
			b := dreamBody(storeID, sessionIDs)
			b["priority"] = "high"
			return b
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := s.do(http.MethodPost, "/v1/dreams", tc.body)
			wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
		})
	}

	// These rules each have a later rule behind them that answers the same 400
	// for a different reason — an id that fails the shape check is also an id
	// no row carries — so the message is what proves which rule fired.
	for _, tc := range []struct {
		name string
		body map[string]any
		msg  string
	}{
		{"no memory_store input", with([]any{sessionsInput}),
			"inputs must carry exactly one memory_store input and one sessions input (found 0 and 1)"},
		{"memory_store_id with the wrong prefix", with([]any{
			map[string]any{"type": "memory_store", "memory_store_id": "vlt_0123456789abcdefghjkmnpq"},
			sessionsInput}),
			`memory_store_id "vlt_0123456789abcdefghjkmnpq" is not a memory store id`},
		{"malformed session id", with([]any{storeInput,
			map[string]any{"type": "sessions", "session_ids": []any{"sesn_NOT-AN-ID"}}}),
			`session_ids entry "sesn_NOT-AN-ID" is not a session id`},
		{"101 session ids", with([]any{storeInput,
			map[string]any{"type": "sessions", "session_ids": many}}),
			"session_ids must carry 1 to 100 session ids, not 101"},
		{"duplicate session id", with([]any{storeInput,
			map[string]any{"type": "sessions", "session_ids": []any{sesn, sesn}}}),
			fmt.Sprintf("session_ids must not repeat %q", sesn)},
		{"duplicate across both spellings", with([]any{storeInput,
			map[string]any{"type": "sessions", "session_ids": []any{sesn, alias}}}),
			fmt.Sprintf("session_ids must not repeat %q", alias)},
		// The arm's key set is checked before its target rules (§5.2): with that
		// check removed this body would 400 on the missing memory_store_id instead.
		{"update_existing carrying an unknown key", dreamWith(storeID, sessionIDs, "output_behavior",
			map[string]any{"type": "update_existing", "extra": 1}),
			`unknown field "extra"`},
		// The target's own rules (§5.3). memory_store_id is required with a
		// minLength of 1 (BetaOutputBehaviorUpdateExisting), and the one value
		// it may take is the dream's own memory_store input — the EAP rule the
		// guide states and this platform infers the wording of.
		{"update_existing with no target", dreamWith(storeID, sessionIDs, "output_behavior",
			map[string]any{"type": "update_existing"}),
			"output_behavior.memory_store_id is required"},
		{"update_existing with a null target", dreamWith(storeID, sessionIDs, "output_behavior",
			map[string]any{"type": "update_existing", "memory_store_id": nil}),
			"output_behavior.memory_store_id is required"},
		{"update_existing with an empty target", dreamWith(storeID, sessionIDs, "output_behavior",
			map[string]any{"type": "update_existing", "memory_store_id": ""}),
			"output_behavior.memory_store_id is required"},
		{"update_existing with a non-string target", dreamWith(storeID, sessionIDs, "output_behavior",
			map[string]any{"type": "update_existing", "memory_store_id": 7}),
			"output_behavior.memory_store_id must be a string"},
		{"update_existing targeting another store", dreamWith(storeID, sessionIDs, "output_behavior",
			map[string]any{"type": "update_existing", "memory_store_id": otherStore}),
			"output_behavior.memory_store_id must be the job's own memory_store input"},
		// A target that exists nowhere takes the same rule, not a not-found:
		// the equality check runs before the store lookup (§5.2's order).
		{"update_existing targeting a store that does not exist", dreamWith(storeID, sessionIDs,
			"output_behavior", map[string]any{"type": "update_existing", "memory_store_id": ghostStore}),
			"output_behavior.memory_store_id must be the job's own memory_store input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := s.do(http.MethodPost, "/v1/dreams", tc.body)
			wantErrMsg(t, status, body, http.StatusBadRequest, "invalid_request_error", tc.msg)
		})
	}

	// 4,096 characters is the bound, not the break.
	createDream(t, s, dreamWith(storeID, sessionIDs, "instructions", strings.Repeat("x", 4096)))
}

// inPlaceDream is the create body of an update_existing dream over its own
// input store — the only target the EAP rule admits (§5.3).
func inPlaceDream(storeID string, sessionIDs []any) map[string]any {
	return dreamWith(storeID, sessionIDs, "output_behavior",
		map[string]any{"type": "update_existing", "memory_store_id": storeID})
}

// settleDreamTerminal puts a dream in a terminal status with closed_at still
// null: the window between a dream ending and the runner's closing arm
// reaching it, and the whole of what the hold has to cover beyond `pending`
// and `running`. The closing arm itself is tested in dreamrunner_test.go; what
// is under test here is the route's answer to each state, so each is written
// directly rather than walked to.
func settleDreamTerminal(t *testing.T, s *tserver, dreamID, status string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE dreams SET status = $2, ended_at = now() WHERE id = $1`, dreamID, status); err != nil {
		t.Fatalf("settle dream %s at %s: %v", dreamID, status, err)
	}
}

// stampDreamClosed is what the closing arm's commit does to the hold.
func stampDreamClosed(t *testing.T, s *tserver, dreamID string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE dreams SET closed_at = now() WHERE id = $1`, dreamID); err != nil {
		t.Fatalf("close dream %s: %v", dreamID, err)
	}
}

// wantTargetHeld asserts the hold's refusal: 409 conflict_error
// (BetaTargetStoreHeldError), the holding dream named in the message, and the
// x-should-retry header the SDK reads instead of retrying a 409 twice.
func wantTargetHeld(t *testing.T, s *tserver, body map[string]any, holder string) {
	t.Helper()
	res := s.doRaw(http.MethodPost, "/v1/dreams", body, map[string]string{"x-api-key": testKey})
	defer res.Body.Close()
	var envelope map[string]any
	if err := json.NewDecoder(res.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode the hold's response: %v", err)
	}
	wantErr(t, res.StatusCode, envelope, http.StatusConflict, "conflict_error")
	inner, _ := envelope["error"].(map[string]any)
	if msg, _ := inner["message"].(string); !strings.Contains(msg, holder) {
		t.Errorf("error.message = %q, want it to name the holding dream %s", msg, holder)
	}
	if got := res.Header.Get("x-should-retry"); got != "false" {
		t.Errorf("x-should-retry = %q, want %q", got, "false")
	}
}

// The hold (§5.3): at most one live update_existing dream per target store,
// and nothing else on that store constrained by it.
func TestDreamUpdateExistingHold(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)

	held := createDream(t, s, inPlaceDream(storeID, sessionIDs))
	if ob, _ := held["output_behavior"].(map[string]any); len(ob) != 2 ||
		ob["type"] != "update_existing" || ob["memory_store_id"] != storeID {
		t.Errorf("output_behavior = %v, want the update_existing arm echoed", held["output_behavior"])
	}
	// The second in-place create on the same store is the 409, not a 500 out
	// of the raw unique violation.
	wantTargetHeld(t, s, inPlaceDream(storeID, sessionIDs), held["id"].(string))

	// A create_new dream holds nothing: its input is read once and its output
	// is its own, so the store takes as many as a caller likes, held or not.
	createDream(t, s, dreamBody(storeID, sessionIDs))
	createDream(t, s, dreamBody(storeID, sessionIDs))

	// And the hold is per target store, not global to the surface.
	other := createMemoryStore(t, s, "dream-second-target")
	createDream(t, s, inPlaceDream(other, sessionIDs))
}

// A create_new dream over a store holds nothing, so an in-place dream may take
// the same store while it runs — the converse of the case above.
func TestDreamCreateNewHoldsNothing(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)

	created := createDream(t, s, dreamBody(storeID, sessionIDs))
	var target *string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT target_memory_store_id FROM dreams WHERE id = $1`, created["id"]).Scan(&target); err != nil {
		t.Fatalf("read target_memory_store_id: %v", err)
	}
	if target != nil {
		t.Errorf("a create_new dream stored target_memory_store_id = %q, want null", *target)
	}
	createDream(t, s, inPlaceDream(storeID, sessionIDs))
}

// The hold outlives the dream's status: every terminal state keeps it until
// the closing arm stamps closed_at, which is the one predicate behind both
// windows the spec names — a cancel whose final writes are still landing, and
// a just-finished dream still closing (§5.3).
func TestDreamHoldSurvivesTerminalUntilClosed(t *testing.T) {
	s := newTestServer(t)
	_, sessionIDs := createDreamInputs(t, s, 1)

	for _, status := range []string{"completed", "failed", "canceled"} {
		t.Run(status, func(t *testing.T) {
			storeID := createMemoryStore(t, s, "dream-hold-"+status)
			holder := createDream(t, s, inPlaceDream(storeID, sessionIDs))["id"].(string)

			settleDreamTerminal(t, s, holder, status)
			wantTargetHeld(t, s, inPlaceDream(storeID, sessionIDs), holder)

			stampDreamClosed(t, s, holder)
			createDream(t, s, inPlaceDream(storeID, sessionIDs))
		})
	}
}

// A dream canceled while `pending` never had a session, so it ends and closes
// in one commit (§5.3) and the target is free the moment the cancel returns.
func TestDreamCancelWhilePendingReleasesTheHold(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)

	holder := createDream(t, s, inPlaceDream(storeID, sessionIDs))["id"].(string)
	wantTargetHeld(t, s, inPlaceDream(storeID, sessionIDs), holder)

	status, body := s.do(http.MethodPost, "/v1/dreams/"+holder+"/cancel", nil)
	if status != http.StatusOK || body["status"] != "canceled" {
		t.Fatalf("cancel: status %d, dream %v", status, body)
	}
	createDream(t, s, inPlaceDream(storeID, sessionIDs))
}

func dreamWithModel(storeID string, sessionIDs []any, model any) map[string]any {
	return map[string]any{"inputs": dreamInputs(storeID, sessionIDs), "model": model}
}

func dreamWith(storeID string, sessionIDs []any, key string, value any) map[string]any {
	b := dreamBody(storeID, sessionIDs)
	b[key] = value
	return b
}

// Two inputs a create accepts: an archived session (only a missing one fails),
// and the session_ wire spelling, echoed back as it was sent.
func TestDreamCreateAcceptsArchivedAndAliasedSessions(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)
	sesn := sessionIDs[0].(string)
	if status, body := s.do(http.MethodPost, "/v1/sessions/"+sesn+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive session: status %d (%v)", status, body)
	}
	createDream(t, s, dreamBody(storeID, sessionIDs))

	alias := "session_" + strings.TrimPrefix(sesn, "sesn_")
	body := createDream(t, s, dreamBody(storeID, []any{alias}))
	inputs, _ := body["inputs"].([]any)
	second, _ := inputs[1].(map[string]any)
	if got, _ := second["session_ids"].([]any); len(got) != 1 || got[0] != alias {
		t.Errorf("session_ids = %v, want the session_ spelling echoed as sent", second["session_ids"])
	}
}

// The documented bounds hold at the boundary and count characters: exactly 100
// session ids are accepted, and so are 4,096 two-byte characters of
// instructions, which a byte count would refuse. An explicit null is read as
// unset on output_behavior and instructions alike — the repo's create-route
// rule, registered in docs/DIVERGENCES.md because the spec types
// output_behavior non-nullable.
func TestDreamCreateBoundsAndNulls(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 100)
	createDream(t, s, dreamBody(storeID, sessionIDs))

	few := sessionIDs[:1]
	createDream(t, s, dreamWith(storeID, few, "instructions", strings.Repeat("é", 4096)))
	status, body := s.do(http.MethodPost, "/v1/dreams",
		dreamWith(storeID, few, "instructions", strings.Repeat("é", 4097)))
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	req := dreamBody(storeID, few)
	req["output_behavior"] = nil
	req["instructions"] = nil
	got := createDream(t, s, req)
	if ob, _ := got["output_behavior"].(map[string]any); len(ob) != 1 || ob["type"] != "create_new" {
		t.Errorf("output_behavior = %v after an explicit null, want {type: create_new}", got["output_behavior"])
	}
	if got["instructions"] != nil {
		t.Errorf("instructions = %v after an explicit null, want null", got["instructions"])
	}
}

func TestDreamGet(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)
	created := createDream(t, s, dreamBody(storeID, sessionIDs))

	status, got := s.do(http.MethodGet, "/v1/dreams/"+created["id"].(string), nil)
	if status != http.StatusOK {
		t.Fatalf("get: status %d (%v)", status, got)
	}
	if fmt.Sprint(got) != fmt.Sprint(created) {
		t.Errorf("get body = %v, want the create body %v", got, created)
	}

	// A well-formed unknown id 404s from the row lookup; a token outside the id
	// alphabet and a NUL byte 404 from checkID, before either reaches Postgres.
	for _, id := range []string{"drm_0123456789abcdefghjkmnpq", "drm_not-an-id", "drm_a%00b"} {
		status, body := s.do(http.MethodGet, "/v1/dreams/"+id, nil)
		wantErr(t, status, body, http.StatusNotFound, "not_found_error")
	}
}

func TestDreamListPaging(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)
	ids := make([]string, 0, 21)
	for range 21 {
		ids = append(ids, createDream(t, s, dreamBody(storeID, sessionIDs))["id"].(string))
	}
	stampCreatedAt(t, s, "dreams", ids...)
	// Three rows share one created_at, straddling the page boundary: a paging
	// test without ties never exercises the (created_at, id) tiebreak the
	// keyset depends on, so it proves nothing about the cursor (the plan 36
	// review's finding), and a tie inside one page would not reach it either.
	tied := ids[0:3]
	if _, err := s.pool.Exec(t.Context(),
		`UPDATE dreams SET created_at = (SELECT created_at FROM dreams WHERE id = $2) WHERE id = ANY($1)`,
		tied, tied[0]); err != nil {
		t.Fatalf("tie three created_at values: %v", err)
	}

	// The default limit is 20, and the cursor carries the walk to the last row.
	_, first := s.do(http.MethodGet, "/v1/dreams", nil)
	page1 := listData(t, first)
	if len(page1) != 20 {
		t.Fatalf("default page holds %d rows, want 20", len(page1))
	}
	cursor := nextPage(t, first)
	if cursor == "" {
		t.Fatal("next_page is empty with a row still unread")
	}
	_, second := s.do(http.MethodGet, "/v1/dreams?page="+url.QueryEscape(cursor), nil)
	page2 := listData(t, second)
	if len(page2) != 1 {
		t.Fatalf("second page holds %d rows, want 1", len(page2))
	}
	if c := nextPage(t, second); c != "" {
		t.Errorf("next_page = %q at the end of the list, want null", c)
	}

	// Every row once, newest first, ids descending within a tie.
	seen := map[string]bool{}
	var prevAt, prevID string
	for i, row := range append(page1, page2...) {
		id, _ := row["id"].(string)
		at, _ := row["created_at"].(string)
		if seen[id] {
			t.Errorf("row %d repeats %s across the page boundary", i, id)
		}
		seen[id] = true
		if i > 0 {
			if at > prevAt || (at == prevAt && id >= prevID) {
				t.Errorf("row %d (%s at %s) does not follow %s at %s in (created_at, id) DESC order",
					i, id, at, prevID, prevAt)
			}
		}
		prevAt, prevID = at, id
	}
	if len(seen) != 21 {
		t.Errorf("the two pages hold %d distinct dreams, want 21", len(seen))
	}

	_, all := s.do(http.MethodGet, "/v1/dreams?limit=100", nil)
	if got := len(listData(t, all)); got != 21 {
		t.Errorf("limit=100 returned %d rows, want 21", got)
	}
	for _, q := range []string{"limit=0", "limit=101", "page=not-a-cursor"} {
		status, body := s.do(http.MethodGet, "/v1/dreams?"+q, nil)
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}
	// One case per arm of the guard: the version, seq and path kinds decode
	// with a zero time and an empty id, so a guard missing any one arm would
	// bind those and answer an empty 200 page — end of history — where a 400
	// belongs; a prev-direction time cursor is the fourth arm.
	for name, cur := range foreignCursors {
		status, body := s.do(http.MethodGet, "/v1/dreams?page="+url.QueryEscape(cur), nil)
		if status != http.StatusBadRequest {
			t.Errorf("%s cursor: status %d (%v), want 400", name, status, body)
		}
	}
}

// The other lists' own cursor encodings — base64url of the k1| grammar page.go's
// decodeCursor reads — which the dreams list must refuse.
var foreignCursors = map[string]string{
	"version":   "azF8bnx2fDE",                                        // k1|n|v|1
	"seq":       "azF8bnxzfGR8NQ",                                     // k1|n|s|d|5
	"path":      "azF8bnxtfFlTOWk",                                    // k1|n|m|base64url("a/b")
	"prev time": "azF8cHx0fDF8ZHJtXzAxMjM0NTY3ODlhYmNkZWZnaGprbW5wcQ", // k1|p|t|1|drm_0123456789abcdefghjkmnpq
}

func TestDreamListFilters(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)
	kept := createDream(t, s, dreamBody(storeID, sessionIDs))["id"].(string)
	canceled := createDream(t, s, dreamBody(storeID, sessionIDs))["id"].(string)
	if status, body := s.do(http.MethodPost, "/v1/dreams/"+canceled+"/cancel", nil); status != http.StatusOK {
		t.Fatalf("cancel: status %d (%v)", status, body)
	}
	archived := createDream(t, s, dreamBody(storeID, sessionIDs))["id"].(string)
	if status, body := s.do(http.MethodPost, "/v1/dreams/"+archived+"/cancel", nil); status != http.StatusOK {
		t.Fatalf("cancel before archive: status %d (%v)", status, body)
	}
	if status, body := s.do(http.MethodPost, "/v1/dreams/"+archived+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d (%v)", status, body)
	}

	listIDs := func(query string) []string {
		t.Helper()
		status, body := s.do(http.MethodGet, "/v1/dreams"+query, nil)
		if status != http.StatusOK {
			t.Fatalf("list%s: status %d (%v)", query, status, body)
		}
		var out []string
		for _, row := range listData(t, body) {
			out = append(out, row["id"].(string))
		}
		return out
	}
	has := func(ids []string, id string) bool { return strings.Contains(strings.Join(ids, ","), id) }

	if ids := listIDs(""); has(ids, archived) || !has(ids, kept) {
		t.Errorf("default list = %v, want the archived dream hidden and the live one shown", ids)
	}
	if ids := listIDs("?include_archived=true"); !has(ids, archived) {
		t.Errorf("include_archived=true list = %v, want the archived dream shown", ids)
	}
	// Both spellings of the status filter, bracketed and bare.
	if ids := listIDs("?statuses[]=pending&statuses[]=canceled"); !has(ids, kept) || !has(ids, canceled) {
		t.Errorf("statuses[] list = %v, want both the pending and the canceled dream", ids)
	}
	if ids := listIDs("?statuses=canceled"); has(ids, kept) || !has(ids, canceled) {
		t.Errorf("statuses=canceled list = %v, want the canceled dream alone", ids)
	}
	status, body := s.do(http.MethodGet, "/v1/dreams?statuses=bogus", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	// Both created_at bounds are exclusive, unlike the memory-store list's
	// [gte]/[lte] pair (betadream.go:923-946).
	_, one := s.do(http.MethodGet, "/v1/dreams/"+kept, nil)
	at := url.QueryEscape(one["created_at"].(string))
	if ids := listIDs("?created_at[gt]=" + at); has(ids, kept) {
		t.Errorf("created_at[gt] at the row's own timestamp returned it: %v", ids)
	}
	if ids := listIDs("?created_at[lt]=" + at); has(ids, kept) {
		t.Errorf("created_at[lt] at the row's own timestamp returned it: %v", ids)
	}
}

func TestDreamArchive(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)
	id := createDream(t, s, dreamBody(storeID, sessionIDs))["id"].(string)

	// A live dream is cancelled first; the guide says so in as many words.
	status, body := s.do(http.MethodPost, "/v1/dreams/"+id+"/archive", nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")

	s.do(http.MethodPost, "/v1/dreams/"+id+"/cancel", nil)
	status, first := s.do(http.MethodPost, "/v1/dreams/"+id+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive: status %d (%v)", status, first)
	}
	if first["archived_at"] == nil {
		t.Errorf("archived_at is null after archiving: %v", first)
	}
	if first["status"] != "canceled" {
		t.Errorf("status = %v after archiving, want the terminal status unchanged", first["status"])
	}
	status, again := s.do(http.MethodPost, "/v1/dreams/"+id+"/archive", nil)
	if status != http.StatusOK || again["archived_at"] != first["archived_at"] {
		t.Errorf("re-archive: status %d, archived_at %v, want 200 and %v",
			status, again["archived_at"], first["archived_at"])
	}

	// The two terminal states no route can reach in slice 1, planted directly.
	for _, terminal := range []string{"completed", "failed"} {
		planted := createDream(t, s, dreamBody(storeID, sessionIDs))["id"].(string)
		if _, err := s.pool.Exec(t.Context(),
			`UPDATE dreams SET status = $2, ended_at = now() WHERE id = $1`, planted, terminal); err != nil {
			t.Fatalf("plant a %s dream: %v", terminal, err)
		}
		status, body := s.do(http.MethodPost, "/v1/dreams/"+planted+"/archive", nil)
		if status != http.StatusOK || body["archived_at"] == nil || body["status"] != terminal {
			t.Errorf("archive a %s dream: status %d (%v)", terminal, status, body)
		}
	}

	status, body = s.do(http.MethodPost, "/v1/dreams/drm_0123456789abcdefghjkmnpq/archive", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestDreamCancel(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)
	id := createDream(t, s, dreamBody(storeID, sessionIDs))["id"].(string)

	status, body := s.do(http.MethodPost, "/v1/dreams/"+id+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel: status %d (%v)", status, body)
	}
	if body["status"] != "canceled" || body["ended_at"] == nil {
		t.Fatalf("cancel body = %v, want canceled with ended_at set", body)
	}
	// A dream that ends without ever having a session has nothing to wind down,
	// so it closes in the same commit (§5.3).
	var closedAt *string
	if err := s.pool.QueryRow(t.Context(),
		`SELECT closed_at::text FROM dreams WHERE id = $1`, id).Scan(&closedAt); err != nil {
		t.Fatalf("read closed_at: %v", err)
	}
	if closedAt == nil {
		t.Error("closed_at is null after cancelling a dream that never had a session")
	}

	status, again := s.do(http.MethodPost, "/v1/dreams/"+id+"/cancel", nil)
	if status != http.StatusOK || again["ended_at"] != body["ended_at"] {
		t.Errorf("re-cancel: status %d, ended_at %v, want 200 and %v",
			status, again["ended_at"], body["ended_at"])
	}

	for _, terminal := range []string{"completed", "failed"} {
		planted := createDream(t, s, dreamBody(storeID, sessionIDs))["id"].(string)
		if _, err := s.pool.Exec(t.Context(),
			`UPDATE dreams SET status = $2, ended_at = now() WHERE id = $1`, planted, terminal); err != nil {
			t.Fatalf("plant a %s dream: %v", terminal, err)
		}
		status, body := s.do(http.MethodPost, "/v1/dreams/"+planted+"/cancel", nil)
		wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	}

	status, body = s.do(http.MethodPost, "/v1/dreams/drm_0123456789abcdefghjkmnpq/cancel", nil)
	wantErr(t, status, body, http.StatusNotFound, "not_found_error")
}

func TestDreamMethodNotAllowed(t *testing.T) {
	s := newTestServer(t)
	storeID, sessionIDs := createDreamInputs(t, s, 1)
	id := createDream(t, s, dreamBody(storeID, sessionIDs))["id"].(string)

	for _, call := range []struct{ method, path string }{
		{http.MethodPut, "/v1/dreams"},
		{http.MethodDelete, "/v1/dreams"},
		{http.MethodPut, "/v1/dreams/" + id},
		{http.MethodPut, "/v1/dreams/" + id + "/archive"},
		{http.MethodGet, "/v1/dreams/" + id + "/archive"},
		{http.MethodPut, "/v1/dreams/" + id + "/cancel"},
		{http.MethodGet, "/v1/dreams/" + id + "/cancel"},
	} {
		status, body := s.do(call.method, call.path, nil)
		wantErr(t, status, body, http.StatusMethodNotAllowed, "invalid_request_error")
	}
}
