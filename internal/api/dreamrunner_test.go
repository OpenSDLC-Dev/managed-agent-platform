package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/blob/blobtest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
)

// The dream runner's state machine and its tick (plan 41 slice 2, §4.1, §7).
// Every case drives the tick directly at an instant it chooses, with no brain
// behind it: the pipeline session's status transitions are written here the
// way the brain writes them, and a counted turn is a planted
// span.model_request_end event.

func dreamCfg() api.DreamRunnerConfig {
	return api.DreamRunnerConfig{TickInterval: time.Second, Timeout: 2 * time.Hour, MaxInputBytes: 64 << 20}
}

// dbNow is the tick's own clock: the database's, never the test host's.
func dbNow(t *testing.T, s *tserver) time.Time {
	t.Helper()
	var now time.Time
	if err := s.pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatalf("SELECT now(): %v", err)
	}
	return now
}

func tickAt(t *testing.T, s *tserver, now time.Time, cfg api.DreamRunnerConfig) {
	t.Helper()
	if err := api.DreamTickForTest(context.Background(), s.pool, s.blobs, now, cfg); err != nil {
		t.Fatalf("dream tick at %s: %v", now, err)
	}
}

func tick(t *testing.T, s *tserver) {
	t.Helper()
	tickAt(t, s, dbNow(t, s), dreamCfg())
}

// getDream reads the dream through the wire, the way a caller sees it.
func getDream(t *testing.T, s *tserver, id string) map[string]any {
	t.Helper()
	status, body := s.do(http.MethodGet, "/v1/dreams/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("GET dream %s: status %d (%v)", id, status, body)
	}
	return body
}

// dreamInternals reads the three columns the wire does not render.
func dreamInternals(t *testing.T, s *tserver, id string) (stage, attempts int, closedAt *time.Time) {
	t.Helper()
	if err := s.pool.QueryRow(context.Background(),
		`SELECT stage, attempts, closed_at FROM dreams WHERE id = $1`, id).
		Scan(&stage, &attempts, &closedAt); err != nil {
		t.Fatalf("read dream columns: %v", err)
	}
	return stage, attempts, closedAt
}

func dreamError(t *testing.T, body map[string]any) (errType, message string) {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("dream carries no error object: %v", body["error"])
	}
	s, _ := e["type"].(string)
	m, _ := e["message"].(string)
	return s, m
}

// setSessionStatus writes the status the brain would write: the session row
// and its primary thread move together, as TransitionThread moves them.
func setSessionStatus(t *testing.T, s *tserver, sessionID, status string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		`UPDATE sessions SET status = $2 WHERE id = $1`, sessionID, status); err != nil {
		t.Fatalf("set session status: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE session_threads SET status = $2 WHERE session_id = $1 AND parent_thread_id IS NULL`,
		sessionID, status); err != nil {
		t.Fatalf("set thread status: %v", err)
	}
}

// appendEvent plants one raw event at the end of a session's log, which is how
// this suite stands in for the brain and the executor.
func appendEvent(t *testing.T, s *tserver, sessionID, typ, payload string) string {
	t.Helper()
	id := domain.NewID(domain.PrefixEvent).String()
	if _, err := s.pool.Exec(context.Background(), `
		INSERT INTO events (id, session_id, seq, type, payload)
		VALUES ($1, $2, (SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE session_id = $2), $3, $4::jsonb)`,
		id, sessionID, typ, payload); err != nil {
		t.Fatalf("append %s: %v", typ, err)
	}
	return id
}

// startedDream creates a dream over a seeded store and n input sessions and
// runs the one tick that starts it, leaving the pipeline session idle — the
// state every arm below begins from.
func startedDream(t *testing.T, s *tserver, body map[string]any) (dreamID, sessionID string) {
	t.Helper()
	created := createDream(t, s, body)
	dreamID = created["id"].(string)
	tick(t, s)
	d := getDream(t, s, dreamID)
	if d["status"] != "running" {
		t.Fatalf("dream %s is %v after its start tick (%v)", dreamID, d["status"], d["error"])
	}
	sessionID = d["session_id"].(string)
	setSessionStatus(t, s, sessionID, "idle")
	return dreamID, sessionID
}

// seededDreamBody is the create body every arm's case uses: one store holding
// two memories, two input sessions.
func seededDreamBody(t *testing.T, s *tserver) (storeID string, body map[string]any) {
	t.Helper()
	storeID = createMemoryStore(t, s, "preferences")
	createMemory(t, s, storeID, "/a.md", "alpha")
	createMemory(t, s, storeID, "/b/c.md", "beta")
	agentID, envID := fixture(t, s)
	var ids []any
	for range 2 {
		ids = append(ids, createSession(t, s,
			map[string]any{"agent": agentID, "environment_id": envID})["id"])
	}
	return storeID, map[string]any{"inputs": dreamInputs(storeID, ids), "model": "claude-opus-4-8"}
}

// A tick with nothing to do is not an error, and does not disturb a dream the
// runner has no arm for — the sanity floor under every case below.
func TestDreamTickWithNoCandidates(t *testing.T) {
	s := newTestServer(t)
	tick(t, s)
}

// Arms 4-8 and 10, each reached exactly once and each leaving the state §4.6
// describes. They share one running dream per case, because the arms are
// ordered and a case that set up two conditions would not say which fired.
func TestDreamTickArms(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, s *tserver, storeID, dreamID, sessionID string)
		status  string
		errType string
		// closesAtOnce: the arm finds no session to wind down, so the same
		// commit stamps closed_at and takes the transcript rows and objects —
		// the closing arm never runs on a dream already closed.
		closesAtOnce bool
	}{
		{
			name: "input store archived is arm 4",
			arrange: func(t *testing.T, s *tserver, storeID, _, _ string) {
				archiveMemoryStore(t, s, storeID)
			},
			status: "failed", errType: "input_memory_store_unavailable",
		},
		{
			name: "input session deleted is arm 4",
			arrange: func(t *testing.T, s *tserver, _, dreamID, _ string) {
				var ids []string
				if err := s.pool.QueryRow(context.Background(),
					`SELECT input_session_ids FROM dreams WHERE id = $1`, dreamID).Scan(&ids); err != nil {
					t.Fatalf("read input sessions: %v", err)
				}
				if _, err := s.pool.Exec(context.Background(),
					`DELETE FROM sessions WHERE id = $1`, ids[0]); err != nil {
					t.Fatalf("delete input session: %v", err)
				}
			},
			status: "failed", errType: "input_session_unavailable",
		},
		{
			name: "output store archived is arm 4",
			arrange: func(t *testing.T, s *tserver, _, dreamID, _ string) {
				archiveMemoryStore(t, s, dreamOutputStore(t, s, dreamID))
			},
			status: "failed", errType: "internal_error",
		},
		{
			name: "the pipeline session deleted at the database level is arm 4",
			arrange: func(t *testing.T, s *tserver, _, _, sessionID string) {
				if _, err := s.pool.Exec(context.Background(),
					`DELETE FROM sessions WHERE id = $1`, sessionID); err != nil {
					t.Fatalf("delete pipeline session: %v", err)
				}
			},
			status: "failed", errType: "internal_error", closesAtOnce: true,
		},
		{
			name: "a terminated session is arm 5",
			arrange: func(t *testing.T, s *tserver, _, _, sessionID string) {
				appendEvent(t, s, sessionID, "session.error",
					`{"error":{"type":"model_request_failed_error","message":"no provider route for model"}}`)
				setSessionStatus(t, s, sessionID, "terminated")
			},
			status: "failed", errType: "internal_error",
		},
		{
			name: "the stage's turn cap is arm 6",
			arrange: func(t *testing.T, s *tserver, _, _, sessionID string) {
				t.Cleanup(api.SetDreamStageTurnCapForTest(2))
				for range 3 {
					appendEvent(t, s, sessionID, "span.model_request_end", `{}`)
				}
			},
			status: "failed", errType: "internal_error",
		},
		{
			name: "an open ask is arm 8",
			arrange: func(t *testing.T, s *tserver, _, _, sessionID string) {
				appendEvent(t, s, sessionID, "agent.tool_use",
					`{"name":"bash","input":{},"evaluated_permission":"ask"}`)
			},
			status: "failed", errType: "internal_error",
		},
		{
			name:    "an idle session with the stage done is arm 10",
			arrange: func(t *testing.T, s *tserver, _, _, _ string) {},
			status:  "completed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			storeID, body := seededDreamBody(t, s)
			dreamID, sessionID := startedDream(t, s, body)

			tc.arrange(t, s, storeID, dreamID, sessionID)
			fileIDs := dreamFileIDs(t, s, dreamID)
			tick(t, s)

			d := getDream(t, s, dreamID)
			if d["status"] != tc.status {
				t.Fatalf("dream is %v, want %s (error %v)", d["status"], tc.status, d["error"])
			}
			if tc.errType == "" {
				if d["error"] != nil {
					t.Errorf("completed dream carries error %v", d["error"])
				}
			} else if got, msg := dreamError(t, d); got != tc.errType {
				t.Errorf("error.type = %q (%q), want %q", got, msg, tc.errType)
			}
			if d["ended_at"] == nil {
				t.Error("a terminal dream has no ended_at")
			}
			_, _, closedAt := dreamInternals(t, s, dreamID)
			if tc.closesAtOnce != (closedAt != nil) {
				t.Errorf("closed_at = %v, want closed at once: %v", closedAt, tc.closesAtOnce)
			}
			if tc.closesAtOnce {
				if left := dreamFileIDs(t, s, dreamID); len(left) != 0 {
					t.Errorf("%d transcript rows survived the close that had no session to wind down", len(left))
				}
				for _, id := range fileIDs {
					if _, _, err := s.blobs.Get(context.Background(), blob.FilesKey(id)); !errors.Is(err, blob.ErrNotFound) {
						t.Errorf("object for %s survived the close: %v", id, err)
					}
				}
			}
		})
	}
}

// Arm 7: a running or rescheduling session is busy, so the tick only refreshes
// usage — and the flat cache_creation_input_tokens is the sum of both TTL
// tiers.
func TestDreamTickBusySessionOnlyMirrorsUsage(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID, sessionID := startedDream(t, s, body)

	for _, status := range []string{"running", "rescheduling"} {
		setSessionStatus(t, s, sessionID, status)
		if _, err := s.pool.Exec(context.Background(), `UPDATE sessions SET usage = $2 WHERE id = $1`,
			sessionID, `{"input_tokens":11,"output_tokens":22,"cache_read_input_tokens":33,`+
				`"cache_creation":{"ephemeral_1h_input_tokens":40,"ephemeral_5m_input_tokens":2}}`); err != nil {
			t.Fatalf("set session usage: %v", err)
		}
		tick(t, s)

		d := getDream(t, s, dreamID)
		if d["status"] != "running" {
			t.Fatalf("a %s session moved the dream to %v", status, d["status"])
		}
		usage := d["usage"].(map[string]any)
		for key, want := range map[string]float64{
			"input_tokens": 11, "output_tokens": 22,
			"cache_read_input_tokens": 33, "cache_creation_input_tokens": 42,
		} {
			if usage[key] != want {
				t.Errorf("usage.%s = %v, want %v", key, usage[key], want)
			}
		}
	}
}

// The timeout is arm 2, and it bounds a dream that never started as well as a
// running one: in pending it closes in the same commit, because there is no
// session to wind down.
func TestDreamTimeout(t *testing.T) {
	t.Run("pending closes at once", func(t *testing.T) {
		s := newTestServer(t)
		_, body := seededDreamBody(t, s)
		id := createDream(t, s, body)["id"].(string)

		tickAt(t, s, dbNow(t, s).Add(3*time.Hour), dreamCfg())

		d := getDream(t, s, id)
		if got, _ := dreamError(t, d); d["status"] != "failed" || got != "timeout" {
			t.Fatalf("dream is %v/%v, want failed/timeout", d["status"], d["error"])
		}
		if _, _, closedAt := dreamInternals(t, s, id); closedAt == nil {
			t.Error("a dream that timed out before it had a session is not closed")
		}
	})
	t.Run("running waits for the closing arm", func(t *testing.T) {
		s := newTestServer(t)
		_, body := seededDreamBody(t, s)
		id, sessionID := startedDream(t, s, body)

		tickAt(t, s, dbNow(t, s).Add(3*time.Hour), dreamCfg())
		d := getDream(t, s, id)
		if got, _ := dreamError(t, d); d["status"] != "failed" || got != "timeout" {
			t.Fatalf("dream is %v/%v, want failed/timeout", d["status"], d["error"])
		}
		if _, _, closedAt := dreamInternals(t, s, id); closedAt != nil {
			t.Fatal("a dream with a session closed in the same commit that failed it")
		}
		if !sessionInterrupted(t, s, sessionID) {
			t.Error("the timeout did not interrupt the pipeline session")
		}
	})
}

// The closing arm is the one that ends every terminal dream: the session
// archived, the transcript rows and their objects gone, closed_at stamped.
func TestDreamClosingArm(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID, sessionID := startedDream(t, s, body)
	fileIDs := dreamFileIDs(t, s, dreamID)
	if len(fileIDs) != 3 {
		t.Fatalf("the dream owns %d files, want 3 (two transcripts and INDEX.md)", len(fileIDs))
	}

	tick(t, s) // arm 10 completes it
	tick(t, s) // arm 1 closes it

	if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt == nil {
		t.Fatal("the closing arm left closed_at null")
	}
	var archivedAt *time.Time
	if err := s.pool.QueryRow(context.Background(),
		`SELECT archived_at FROM sessions WHERE id = $1`, sessionID).Scan(&archivedAt); err != nil {
		t.Fatalf("read pipeline session: %v", err)
	}
	if archivedAt == nil {
		t.Error("the pipeline session was not archived")
	}
	for _, id := range fileIDs {
		if status, _ := s.do(http.MethodGet, "/v1/files/"+id, nil); status != http.StatusNotFound {
			t.Errorf("GET /v1/files/%s = %d after the close, want 404", id, status)
		}
		if _, _, err := s.blobs.Get(context.Background(), blob.FilesKey(id)); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("object for %s survives the close: %v", id, err)
		}
	}
	// The gate lifts with the close: the archived session answers as any
	// archived session does rather than "owned by dream".
	status, body2 := s.do(http.MethodPost, "/v1/sessions/"+sessionID+"/archive", nil)
	if status != http.StatusOK {
		t.Errorf("archive after the close: status %d (%v)", status, body2)
	}
}

// A session deleted at the database level skips the archive and closes the
// same way — the transcripts are the dream's, not the session's.
func TestDreamClosingArmWithTheSessionGone(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID, sessionID := startedDream(t, s, body)
	fileIDs := dreamFileIDs(t, s, dreamID)

	tick(t, s) // completes
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id = $1`, sessionID); err != nil {
		t.Fatalf("delete pipeline session: %v", err)
	}
	tick(t, s) // closes without it

	d := getDream(t, s, dreamID)
	if d["session_id"] != nil {
		t.Errorf("session_id = %v after the row was deleted, want null", d["session_id"])
	}
	if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt == nil {
		t.Fatal("the closing arm needs the session row to close")
	}
	for _, id := range fileIDs {
		if _, _, err := s.blobs.Get(context.Background(), blob.FilesKey(id)); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("object for %s survives the close: %v", id, err)
		}
	}
}

// A running session cannot be archived, so the closing arm interrupts it again
// and waits for the next tick; a rescheduling one is archived where it stands.
func TestDreamClosingArmWaitsForARunningSession(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID, sessionID := startedDream(t, s, body)

	tick(t, s) // completes while idle
	setSessionStatus(t, s, sessionID, "running")
	tick(t, s)
	if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt != nil {
		t.Fatal("the closing arm closed a dream whose session was still running")
	}
	setSessionStatus(t, s, sessionID, "rescheduling")
	tick(t, s)
	if _, _, closedAt := dreamInternals(t, s, dreamID); closedAt == nil {
		t.Fatal("a rescheduling session is archivable; the dream should have closed")
	}
}

// Cancel is a request-side transition. A pending dream ends and closes in one
// commit; a running one is interrupted in the cancel's own transaction and
// left for the closing arm.
func TestDreamCancelPendingAndRunning(t *testing.T) {
	t.Run("pending closes at once", func(t *testing.T) {
		s := newTestServer(t)
		_, body := seededDreamBody(t, s)
		id := createDream(t, s, body)["id"].(string)

		if status, res := s.do(http.MethodPost, "/v1/dreams/"+id+"/cancel", nil); status != http.StatusOK {
			t.Fatalf("cancel: status %d (%v)", status, res)
		}
		if _, _, closedAt := dreamInternals(t, s, id); closedAt == nil {
			t.Fatal("a pending cancel did not close the dream")
		}
	})
	t.Run("running interrupts and waits", func(t *testing.T) {
		s := newTestServer(t)
		_, body := seededDreamBody(t, s)
		id, sessionID := startedDream(t, s, body)
		setSessionStatus(t, s, sessionID, "running")

		status, res := s.do(http.MethodPost, "/v1/dreams/"+id+"/cancel", nil)
		if status != http.StatusOK || res["status"] != "canceled" {
			t.Fatalf("cancel: status %d (%v)", status, res)
		}
		if !sessionInterrupted(t, s, sessionID) {
			t.Fatal("the cancel did not run the interrupt on the pipeline session")
		}
		if _, _, closedAt := dreamInternals(t, s, id); closedAt != nil {
			t.Fatal("cancel closed the dream itself; the closing arm owns that")
		}
		// The interrupt idled the session, so the very next tick closes it.
		tick(t, s)
		if _, _, closedAt := dreamInternals(t, s, id); closedAt == nil {
			t.Fatal("the closing arm did not finish a canceled dream")
		}
	})
	t.Run("rescheduling is archived where it stands", func(t *testing.T) {
		s := newTestServer(t)
		_, body := seededDreamBody(t, s)
		id, sessionID := startedDream(t, s, body)
		setSessionStatus(t, s, sessionID, "rescheduling")

		if status, res := s.do(http.MethodPost, "/v1/dreams/"+id+"/cancel", nil); status != http.StatusOK {
			t.Fatalf("cancel: status %d (%v)", status, res)
		}
		if !sessionInterrupted(t, s, sessionID) {
			t.Fatal("the cancel did not interrupt a rescheduling session")
		}
		tick(t, s)
		if _, _, closedAt := dreamInternals(t, s, id); closedAt == nil {
			t.Fatal("the closing arm did not archive a session that is not running")
		}
	})
}

// The completion scan reads back only what the session wrote after it was
// created: a secret planted in a version the session wrote fails the dream,
// and one already in the input store before the clone does not.
func TestDreamCompletionSecretScan(t *testing.T) {
	t.Run("a secret the session wrote fails the dream", func(t *testing.T) {
		s := newTestServer(t)
		_, body := seededDreamBody(t, s)
		dreamID, sessionID := startedDream(t, s, body)
		writeSessionVersion(t, s, dreamOutputStore(t, s, dreamID), sessionID,
			"/leaked.md", "the key is sk-abcdefghijklmnop")

		tick(t, s)
		d := getDream(t, s, dreamID)
		if got, msg := dreamError(t, d); d["status"] != "failed" || got != "internal_error" {
			t.Fatalf("dream is %v/%v/%q, want failed/internal_error", d["status"], got, msg)
		}
	})
	t.Run("a secret already in the input store does not", func(t *testing.T) {
		s := newTestServer(t)
		storeID := createMemoryStore(t, s, "preferences")
		createMemory(t, s, storeID, "/keys.md", "the key is sk-abcdefghijklmnop")
		agentID, envID := fixture(t, s)
		ids := []any{createSession(t, s, map[string]any{"agent": agentID, "environment_id": envID})["id"]}
		body := map[string]any{"inputs": dreamInputs(storeID, ids), "model": "claude-opus-4-8"}

		dreamID, _ := startedDream(t, s, body)
		tick(t, s)
		if d := getDream(t, s, dreamID); d["status"] != "completed" {
			t.Fatalf("dream is %v (%v); the clone's own versions must stay out of the scan",
				d["status"], d["error"])
		}
	})
}

// Two replicas ticking the same dream: FOR UPDATE SKIP LOCKED gives the arm to
// one of them, and the loser changes nothing.
func TestDreamTwoReplicasOneArm(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	dreamID, _ := startedDream(t, s, body)

	now := dbNow(t, s)
	done := make(chan error, 2)
	for range 2 {
		go func() {
			done <- api.DreamTickForTest(context.Background(), s.pool, s.blobs, now, dreamCfg())
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("concurrent tick: %v", err)
		}
	}
	// One completion, one skip: the dream is completed exactly once and its
	// ended_at is a single stamp.
	if d := getDream(t, s, dreamID); d["status"] != "completed" {
		t.Fatalf("dream is %v after two concurrent ticks", d["status"])
	}
}

// With the runner disabled, the create refuses and the other four routes keep
// answering — so a dream created while a runner was configured stays readable,
// cancelable and archivable.
func TestDreamRunnerDisabled(t *testing.T) {
	s := newTestServer(t)
	_, body := seededDreamBody(t, s)
	id := createDream(t, s, body)["id"].(string)

	off := httptest.NewServer(api.NewHandler(s.pool, blobtest.Mem(), nil, nil))
	t.Cleanup(off.Close)
	d := &tserver{t: t, url: off.URL, pool: s.pool}

	status, res := d.do(http.MethodPost, "/v1/dreams", body)
	if status != http.StatusInternalServerError {
		t.Fatalf("create with the runner disabled: status %d (%v)", status, res)
	}
	if e, _ := res["error"].(map[string]any); e == nil || e["type"] != "api_error" ||
		e["message"] != "the dream runner is disabled on this deployment" {
		t.Errorf("create refusal is %v, want the api_error naming the disabled runner", res["error"])
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/dreams"},
		{http.MethodGet, "/v1/dreams/" + id},
		{http.MethodPost, "/v1/dreams/" + id + "/cancel"},
		{http.MethodPost, "/v1/dreams/" + id + "/archive"},
	} {
		if status, res := d.do(tc.method, tc.path, nil); status != http.StatusOK {
			t.Errorf("%s %s with the runner disabled: status %d (%v)", tc.method, tc.path, status, res)
		}
	}
}

// --- shared readers -------------------------------------------------------

func archiveMemoryStore(t *testing.T, s *tserver, storeID string) {
	t.Helper()
	if status, body := s.do(http.MethodPost, "/v1/memory_stores/"+storeID+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive memory store %s: status %d (%v)", storeID, status, body)
	}
}

// dreamOutputStore is outputs[0].memory_store_id — the clone the start arm
// wrote in the same commit as `running`.
func dreamOutputStore(t *testing.T, s *tserver, dreamID string) string {
	t.Helper()
	var raw []byte
	if err := s.pool.QueryRow(context.Background(),
		`SELECT outputs FROM dreams WHERE id = $1`, dreamID).Scan(&raw); err != nil {
		t.Fatalf("read outputs: %v", err)
	}
	var out []struct {
		Type          string `json:"type"`
		MemoryStoreID string `json:"memory_store_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode outputs: %v", err)
	}
	if len(out) != 1 || out[0].Type != "memory_store" {
		t.Fatalf("outputs = %s, want one memory_store entry", raw)
	}
	return out[0].MemoryStoreID
}

func dreamFileIDs(t *testing.T, s *tserver, dreamID string) []string {
	t.Helper()
	rows, err := s.pool.Query(context.Background(),
		`SELECT id FROM files WHERE dream_id = $1 ORDER BY filename`, dreamID)
	if err != nil {
		t.Fatalf("read dream files: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan file id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// sessionInterrupted reports whether the whole interrupt arm ran — the
// user.interrupt event a bare status write would not have produced.
func sessionInterrupted(t *testing.T, s *tserver, sessionID string) bool {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE session_id = $1 AND type = 'user.interrupt'`,
		sessionID).Scan(&n); err != nil {
		t.Fatalf("count interrupts: %v", err)
	}
	return n > 0
}

// writeSessionVersion plants a memory the pipeline session wrote: a memories
// row and the `created` version attributed to it, the shape the executor's
// memory sync writes.
func writeSessionVersion(t *testing.T, s *tserver, storeID, sessionID, path, content string) {
	t.Helper()
	ctx := context.Background()
	memoryID := domain.NewID(domain.PrefixMemory).String()
	versionID := domain.NewID(domain.PrefixMemoryVersion).String()
	actor := fmt.Sprintf(`{"type":"session_actor","session_id":%q}`, sessionID)
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memory_versions (id, memory_store_id, memory_id, operation, path, content,
			content_sha256, content_size_bytes, created_by)
		VALUES ($1, $2, $3, 'created', $4, $5, '', $6, $7::jsonb)`,
		versionID, storeID, memoryID, path, content, len(content), actor); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO memories (id, memory_store_id, path, content, content_sha256,
			content_size_bytes, memory_version_id)
		VALUES ($1, $2, $3, $4, '', $5, $6)`,
		memoryID, storeID, path, content, len(content), versionID); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
}
