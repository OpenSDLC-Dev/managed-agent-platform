package api_test

import (
	"fmt"
	"net/http"
	"testing"
)

func wantEffort(t *testing.T, agent map[string]any, level string) {
	t.Helper()
	m, ok := agent["model"].(map[string]any)
	if !ok {
		t.Fatalf("model = %T, want object", agent["model"])
	}
	if level == "" {
		if _, ok := m["effort"]; ok {
			t.Fatalf("unexpected effort: %v", m)
		}
		return
	}
	e, _ := m["effort"].(map[string]any)
	if len(e) != 1 || e["type"] != level {
		t.Fatalf("model = %v, want effort object %q", m, level)
	}
}

func TestAgentEffortPersistence(t *testing.T) {
	s := newTestServer(t)
	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		for _, effort := range []any{level, map[string]any{"type": level}} {
			a := createAgent(t, s, map[string]any{"name": "effort", "model": map[string]any{"id": "custom", "effort": effort}})
			wantEffort(t, a, level)
			_, got := s.do(http.MethodGet, "/v1/agents/"+a["id"].(string), nil)
			wantEffort(t, got, level)
		}
	}
}

func TestAgentEffortUpdatesAndVersions(t *testing.T) {
	s := newTestServer(t)
	a := createAgent(t, s, map[string]any{"name": "effort", "model": map[string]any{"id": "custom", "effort": "high", "speed": "fast"}})
	id := a["id"].(string)
	for _, model := range []any{map[string]any{"id": "custom"}, "custom"} {
		status, got := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"model": model})
		if status != http.StatusOK {
			t.Fatalf("update: %d %v", status, got)
		}
		wantEffort(t, got, "high")
		if _, present := got["model"].(map[string]any)["speed"]; present {
			t.Fatal("speed should still be replaced")
		}
	}
	status, got := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"model": "other-model"})
	if status != http.StatusOK {
		t.Fatalf("model change: %d %v", status, got)
	}
	wantEffort(t, got, "")
	status, got = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"model": map[string]any{"id": "other-model", "effort": map[string]any{"type": "low"}}})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, got)
	}
	wantEffort(t, got, "low")
	status, got = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"model": map[string]any{"id": "other-model", "effort": "low"}})
	if status != http.StatusOK {
		t.Fatalf("string effort update: %d %v", status, got)
	}
	wantEffort(t, got, "low")
	_, pinned := s.do(http.MethodGet, "/v1/agents/"+id+"?version=1", nil)
	wantEffort(t, pinned, "high")
}

func TestEffortInvalidAtEveryAPIBoundary(t *testing.T) {
	s := newTestServer(t)
	id, env := fixture(t, s)
	for _, effort := range []any{nil, "", "ultra", 1, true, []any{}, map[string]any{}, map[string]any{"type": nil}, map[string]any{"type": "invalid"}, map[string]any{"type": 1}} {
		t.Run(fmt.Sprint(effort), func(t *testing.T) {
			model := map[string]any{"id": "custom", "effort": effort}
			status, got := s.do(http.MethodPost, "/v1/agents", map[string]any{"name": "invalid", "model": model})
			wantErr(t, status, got, http.StatusBadRequest, "invalid_request_error")
			status, got = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"model": model})
			wantErr(t, status, got, http.StatusBadRequest, "invalid_request_error")
			status, got = s.do(http.MethodPost, "/v1/sessions", map[string]any{"environment_id": env, "agent": map[string]any{"type": "agent_with_overrides", "id": id, "model": model}})
			wantErr(t, status, got, http.StatusBadRequest, "invalid_request_error")
		})
	}
}

func TestSessionEffortOverrides(t *testing.T) {
	s := newTestServer(t)
	id, env := fixture(t, s)
	status, got := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"model": map[string]any{"id": "custom", "effort": "high"}})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, got)
	}
	for _, tc := range []struct {
		model any
		want  string
	}{
		{nil, "high"}, {map[string]any{"id": "custom", "effort": "low"}, "low"}, {map[string]any{"id": "custom", "effort": map[string]any{"type": "max"}}, "max"}, {"other", ""},
	} {
		a := map[string]any{"type": "agent_with_overrides", "id": id}
		if tc.model != nil {
			a["model"] = tc.model
		}
		session := createSession(t, s, map[string]any{"agent": a, "environment_id": env})
		wantEffort(t, session["agent"].(map[string]any), tc.want)
		_, saved := s.do(http.MethodGet, "/v1/sessions/"+session["id"].(string), nil)
		wantEffort(t, saved["agent"].(map[string]any), tc.want)
	}
	_, base := s.do(http.MethodGet, "/v1/agents/"+id, nil)
	wantEffort(t, base, "high")
}
