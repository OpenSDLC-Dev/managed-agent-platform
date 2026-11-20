package executor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api/apitest"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/queue"
)

func TestCloudInterruptHarvest(t *testing.T) {
	const filename, contents = "report.txt", "finished before the interrupt"
	for _, tc := range []struct {
		name      string
		grading   bool
		redirect  bool
		wantFiles int
		wantTurns int
	}{
		{name: "plain session", wantFiles: 1},
		{name: "grading cycle", grading: true},
		{name: "redirect in the same batch", redirect: true, wantTurns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, &fakeSandbox{files: map[string]string{outputsDir + "/" + filename: contents}})
			h.prov.markRunning(h.sid)
			if tc.grading {
				h.seedOutcome(t, domain.OutcomeResultEvaluating)
				h.enqueueHarvest(t)
			} else if _, err := h.queue.Enqueue(t.Context(), h.pool, h.envID, h.sid, queue.ModelTurn); err != nil {
				t.Fatal(err)
			}

			handler := api.NewHandler(h.pool, h.blobs, h.cipher, nil)
			key := apitest.OwnedKey(t, h.pool)
			request := func(method, path, body string) *httptest.ResponseRecorder {
				t.Helper()
				req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
				req.Header.Set("x-api-key", key)
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
				}
				return rec
			}
			batch := `{"events":[{"type":"user.interrupt"}`
			if tc.redirect {
				batch += `,{"type":"user.message","content":[{"type":"text","text":"keep working"}]}`
			}
			request(http.MethodPost, "/v1/sessions/"+h.sid.String()+"/events", batch+`]}`)

			worked, err := h.exec.step(t.Context())
			if err != nil || worked != (tc.wantFiles > 0) {
				t.Errorf("executor step = %v, %v, want harvest work iff publishing", worked, err)
			}
			if got := h.liveOf(t, queue.ModelTurn); got != tc.wantTurns {
				t.Errorf("live model turns = %d, want %d", got, tc.wantTurns)
			}
			if h.prov.provisions != 0 {
				t.Errorf("Provision called %d times, want 0", h.prov.provisions)
			}

			rec := request(http.MethodGet, "/v1/files?scope_id="+h.sid.String(), "")
			var files struct {
				Data []struct{ ID, Filename string }
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &files); err != nil {
				t.Fatal(err)
			}
			if len(files.Data) != tc.wantFiles {
				t.Fatalf("session files = %+v, want %d", files.Data, tc.wantFiles)
			}
			for _, file := range files.Data {
				if file.Filename != filename {
					t.Errorf("filename = %q, want %q", file.Filename, filename)
				}
				res := request(http.MethodGet, "/v1/files/"+file.ID+"/content", "")
				if got := res.Body.String(); got != contents {
					t.Errorf("download = %q, want %q", got, contents)
				}
			}
		})
	}
}
