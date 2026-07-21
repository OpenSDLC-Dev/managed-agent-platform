package api_test

import (
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// skillUploadCount sums the skills.uploads counter for one outcome label.
func skillUploadCount(t *testing.T, rm metricdata.ResourceMetrics, outcome string) int64 {
	t.Helper()
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != api.MetricSkillUploads {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", api.MetricSkillUploads, m.Data)
			}
			for _, p := range sum.DataPoints {
				if v, ok := p.Attributes.Value("outcome"); ok && v.AsString() == outcome {
					total += p.Value
				}
			}
		}
	}
	return total
}

// skillBytePoints returns the histogram points of one of the byte instruments.
func skillBytePoints(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.HistogramDataPoint[int64] {
	t.Helper()
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 histogram", name, m.Data)
			}
			return h.DataPoints
		}
	}
	return nil
}

func TestSkillUploadAndDownloadMetrics(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)

	created := s.createSkill(t) // one ok upload
	ct, body := skillForm(t, nil, []upFile{{name: "SKILL.md", content: testSkillMD}})
	if status, _ := s.doForm("POST", "/v1/skills", ct, body); status != http.StatusBadRequest {
		t.Fatalf("flat-basename upload = %d, want 400", status)
	}
	id, _ := created["id"].(string)
	version, _ := created["latest_version"].(string)
	res := s.doRaw("GET", "/v1/skills/"+id+"/versions/"+version+"/content", nil,
		map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", res.StatusCode)
	}

	rm := collect()
	if got := skillUploadCount(t, rm, "ok"); got != 1 {
		t.Errorf("uploads{outcome=ok} = %d, want 1", got)
	}
	if got := skillUploadCount(t, rm, "invalid"); got != 1 {
		t.Errorf("uploads{outcome=invalid} = %d, want 1", got)
	}
	up := skillBytePoints(t, rm, api.MetricSkillUploadBytes)
	if len(up) != 1 || up[0].Count != 1 || up[0].Sum <= 0 {
		t.Errorf("upload.bytes = %+v, want one positive reading", up)
	}
	down := skillBytePoints(t, rm, api.MetricSkillDownloadBytes)
	if len(down) != 1 || down[0].Count != 1 || down[0].Sum <= 0 {
		t.Errorf("download.bytes = %+v, want one positive reading", down)
	}
}

// A crafted seq-type cursor (the session-events keyset) must be rejected by
// the skills lists like every other wrong-typed cursor, not silently bind a
// zero position.
func TestSkillListsRejectSeqCursor(t *testing.T) {
	s := newTestServer(t)
	created := s.createSkill(t)
	id, _ := created["id"].(string)
	seqCursor := base64.RawURLEncoding.EncodeToString([]byte("k1|n|s|a|5"))

	status, body := s.do("GET", "/v1/skills?page="+seqCursor, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
	status, body = s.do("GET", "/v1/skills/"+id+"/versions?page="+seqCursor, nil)
	wantErr(t, status, body, http.StatusBadRequest, "invalid_request_error")
}
