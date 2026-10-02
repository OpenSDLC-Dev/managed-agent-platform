package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

// TestACloudEnvironmentKeyCannotTakeItsWork pins what a key issued on a cloud
// environment reaches on the work API, now that the console issues one there
// as the reference does (#820). A cloud environment's work is run in-process by
// the platform executor, so a worker holding such a key must never take it.
// The reference answers that worker's poll with a 400, its work listing with a
// 404 and its stats with a 200 whose workers_polling is null, archived or not
// (2026-09-05 batch2 `rec83.active-key.work.*.beta`, idx 37–39, and
// `rec83.archived-key.work.*.beta`, idx 33–35; batch8
// `item2.workpoll.envkey.cloud-env-rejected`, idx 24), and so do we. Every
// item route answers the cloud item with the work-item 404, which no recording
// reaches, and the item stays the executor's.
func TestACloudEnvironmentKeyCannotTakeItsWork(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	agent := createAgent(t, s, map[string]any{"name": "cloud-work", "model": "claude-opus-4-8"})

	for _, archived := range []bool{false, true} {
		name := "active cloud environment"
		if archived {
			name = "archived cloud environment"
		}
		t.Run(name, func(t *testing.T) {
			envID := createEnvironment(t, s, map[string]any{"name": "rec83-cloud-work"})["id"].(string)
			sessionID := enqueueToolExec(t, s, agent["id"].(string), envID)
			var workID string
			if err := s.pool.QueryRow(ctx,
				`SELECT id FROM work_items WHERE session_id = $1 AND kind = 'tool_exec'`, sessionID).Scan(&workID); err != nil {
				t.Fatalf("read the cloud item: %v", err)
			}
			if archived {
				if status, body := s.do(http.MethodPost, "/v1/environments/"+envID+"/archive", nil); status != http.StatusOK {
					t.Fatalf("archive: status %d, body %v", status, body)
				}
			}
			key := issueViaConsole(t, s, envID, "rec83-cloud")
			bearer := map[string]string{"Authorization": "Bearer " + key, "Anthropic-Worker-ID": "cloud-worker"}
			base := "/v1/environments/" + envID + "/work"

			for _, query := range []string{"", "?block_ms=999"} {
				res, raw := s.pollQuery(t, envID, query, bearer)
				var body map[string]any
				_ = json.Unmarshal([]byte(raw), &body)
				// In the reference's words (#540): batch2 idx 34 and 38, batch8 idx 24.
				wantErrMsg(t, res.StatusCode, body, http.StatusBadRequest, "invalid_request_error",
					"Only BYOC and bridge environments support work polling. Environment "+envID+" is anthropic_cloud.")
				wantDetails(t, body, nil)
			}
			// Refused before anything is recorded: the polls carried a worker id,
			// and none of them counts toward workers_polling.
			var polls int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM worker_polls WHERE environment_id = $1`, envID).Scan(&polls); err != nil {
				t.Fatalf("count worker polls: %v", err)
			}
			if polls != 0 {
				t.Errorf("worker_polls holds %d rows for the cloud environment after its refused polls; want none", polls)
			}

			// batch2 idx 37 `rec83.active-key.work.list.beta`, verbatim (#540).
			// The archived environment's listing answered "Environment … is
			// archived" there (idx 33): an archival check this route does not make.
			status, body := readJSON(t, s.doRaw(http.MethodGet, base, nil, bearer))
			wantErrMsg(t, status, body, http.StatusNotFound, "not_found_error",
				"Environment `"+envID+"` not found, or does not support work listing. The work API is only available for self-hosted environments.")
			wantDetails(t, body, nil)

			status, body = readJSON(t, s.doRaw(http.MethodGet, base+"/stats", nil, bearer))
			if status != http.StatusOK {
				t.Fatalf("stats: status %d, body %v; want 200", status, body)
			}
			if v, ok := body["workers_polling"]; !ok || v != nil {
				t.Errorf("workers_polling = %v (present %v), want null", v, ok)
			}
			if body["depth"] != float64(0) || body["pending"] != float64(0) || body["oldest_queued_at"] != nil {
				t.Errorf("stats = %v, want an empty queue: the cloud item is not a worker's", body)
			}

			item := base + "/" + workID
			for route, req := range map[string]struct {
				method, path string
				body         any
			}{
				"get":       {http.MethodGet, item, nil},
				"update":    {http.MethodPost, item, map[string]any{"metadata": map[string]any{"k": "v"}}},
				"ack":       {http.MethodPost, item + "/ack", nil},
				"heartbeat": {http.MethodPost, item + "/heartbeat?expected_last_heartbeat=2026-09-05T00:00:00Z", nil},
				"stop":      {http.MethodPost, item + "/stop", map[string]any{"force": true}},
			} {
				t.Run(route, func(t *testing.T) {
					status, body := readJSON(t, s.doRaw(req.method, req.path, req.body, bearer))
					wantErr(t, status, body, http.StatusNotFound, "not_found_error")
				})
			}

			// The item is as it was enqueued — no reservation, no ack, no
			// metadata — and the executor's in-process claim takes it.
			var state string
			var lease *time.Time
			var metadata []byte
			if err := s.pool.QueryRow(ctx,
				`SELECT state, lease_expires_at, metadata FROM work_items WHERE id = $1`, workID).Scan(&state, &lease, &metadata); err != nil {
				t.Fatalf("read the cloud item back: %v", err)
			}
			if state != "queued" || lease != nil || strings.Contains(string(metadata), `"k"`) {
				t.Errorf("cloud item after the worker's attempts: state %q, lease %v, metadata %s; want it untouched", state, lease, metadata)
			}
			claimed, err := queue.New(s.pool).Claim(ctx, queue.ToolExec, time.Minute)
			if err != nil {
				t.Fatalf("executor claim: %v", err)
			}
			if claimed == nil || claimed.ID.String() != workID {
				t.Errorf("executor claimed %v, want the cloud item %s", claimed, workID)
			}
		})
	}
}

// TestAnArchivedSelfHostedEnvironmentStillDrains pins the inference that keeps
// archival out of the poll: an archived self_hosted environment's key still
// polls its queue and is handed the work already in it. The reference's own
// refusal to delete such an environment tells the operator to "archive the
// environment first to allow the queue to drain" (2026-09-02, the DELETE
// refusal #546 matched), and its poll on an archived environment was recorded
// only on a cloud one, where it answered the cloud 400 without looking at
// archival (2026-09-05 batch2 `rec83.archived-key.work.poll.beta`, idx 34).
// The listing and stats stay served too; the reference's listing refused an
// archived cloud environment (idx 33), and whether it refuses an archived
// self_hosted one is unrecorded.
func TestAnArchivedSelfHostedEnvironmentStillDrains(t *testing.T) {
	s := newTestServer(t)
	envID, sessionID, _ := selfHostedWorker(t, s, "drain")
	if _, err := queue.New(s.pool).Enqueue(context.Background(), s.pool, domain.ID(envID), domain.ID(sessionID), queue.ToolExec); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if status, body := s.do(http.MethodPost, "/v1/environments/"+envID+"/archive", nil); status != http.StatusOK {
		t.Fatalf("archive: status %d, body %v", status, body)
	}
	key := issueViaConsole(t, s, envID, "drainer")
	bearer := map[string]string{"Authorization": "Bearer " + key}

	res, raw := s.poll(t, envID, bearer)
	var item map[string]any
	_ = json.Unmarshal([]byte(raw), &item)
	if res.StatusCode != http.StatusOK || item == nil {
		t.Fatalf("poll on an archived self_hosted environment: status %d, body %s; want 200 with its item", res.StatusCode, raw)
	}
	if d, _ := item["data"].(map[string]any); d == nil || d["id"] != sessionID {
		t.Errorf("polled %v, want the archived environment's own session's item", item)
	}
	for _, path := range []string{"/work", "/work/stats"} {
		status, body := readJSON(t, s.doRaw(http.MethodGet, "/v1/environments/"+envID+path, nil, bearer))
		if status != http.StatusOK {
			t.Errorf("GET %s: status %d, body %v; want 200", path, status, body)
		}
	}
}
