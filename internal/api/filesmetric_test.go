package api_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/api"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fileUploadCount sums the files.uploads counter for one outcome label.
func fileUploadCount(t *testing.T, rm metricdata.ResourceMetrics, outcome string) int64 {
	t.Helper()
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != api.MetricFileUploads {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 sum", api.MetricFileUploads, m.Data)
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

// fileBytePoints returns the histogram points of one of the byte instruments.
func fileBytePoints(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.HistogramDataPoint[int64] {
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

func TestFileUploadAndDownloadMetrics(t *testing.T) {
	collect := collectMetrics(t)
	s := newTestServer(t)
	oct := "application/octet-stream"

	s.uploadFile(t, "ok.bin", &oct, "stored bytes") // one ok upload

	ct, form := fileForm(t, "bad/name.bin", &oct, "x") // one invalid upload (forbidden char)
	if status, _ := s.doForm("POST", "/v1/files", ct, form); status != http.StatusBadRequest {
		t.Fatalf("forbidden-char upload = %d, want 400", status)
	}

	// Seed a downloadable file and download it (the API produces none in slice 1).
	genID := "file_0000000000000000000000mm"
	content := []byte("served bytes")
	if err := s.blobs.Put(context.Background(), "files/"+genID, bytes.NewReader(content), int64(len(content)), "image/png"); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	if _, err := s.pool.Exec(context.Background(),
		`INSERT INTO files (id, filename, mime_type, size_bytes, downloadable) VALUES ($1,$2,$3,$4,true)`,
		genID, "chart.png", "image/png", len(content)); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	res := s.doRaw("GET", "/v1/files/"+genID+"/content", nil, map[string]string{"x-api-key": testKey})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", res.StatusCode)
	}

	rm := collect()
	if got := fileUploadCount(t, rm, "ok"); got != 1 {
		t.Errorf("uploads{outcome=ok} = %d, want 1", got)
	}
	if got := fileUploadCount(t, rm, "invalid"); got != 1 {
		t.Errorf("uploads{outcome=invalid} = %d, want 1", got)
	}
	up := fileBytePoints(t, rm, api.MetricFileUploadBytes)
	if len(up) != 1 || up[0].Count != 1 || up[0].Sum <= 0 {
		t.Errorf("upload.bytes = %+v, want one positive reading", up)
	}
	down := fileBytePoints(t, rm, api.MetricFileDownloadBytes)
	if len(down) != 1 || down[0].Count != 1 || down[0].Sum <= 0 {
		t.Errorf("download.bytes = %+v, want one positive reading", down)
	}
}
