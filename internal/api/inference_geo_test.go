package api_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

func wantInferenceGeo(t *testing.T, agent map[string]any, geo string) {
	t.Helper()
	m, ok := agent["model"].(map[string]any)
	if !ok {
		t.Fatalf("model = %T, want object", agent["model"])
	}
	got, present := m["inference_geo"]
	if geo == "" {
		if present {
			t.Fatalf("unexpected inference_geo: %v", m)
		}
	} else if got != geo {
		t.Fatalf("inference_geo = %v, want %q", got, geo)
	}
}

func TestInferenceGeoInvalidAtEveryAPIBoundary(t *testing.T) {
	s := newTestServer(t)
	id, env := fixture(t, s)
	for _, geo := range []any{"", "eu", 1, true, []any{}, map[string]any{"type": "us"}} {
		t.Run(fmt.Sprint(geo), func(t *testing.T) {
			model := map[string]any{"id": "custom", "inference_geo": geo}
			status, got := s.do(http.MethodPost, "/v1/agents", map[string]any{"name": "invalid", "model": model})
			wantErr(t, status, got, http.StatusBadRequest, "invalid_request_error")
			status, got = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"model": model})
			wantErr(t, status, got, http.StatusBadRequest, "invalid_request_error")
			status, got = s.do(http.MethodPost, "/v1/sessions", map[string]any{"environment_id": env, "agent": map[string]any{"type": "agent_with_overrides", "id": id, "model": model}})
			wantErr(t, status, got, http.StatusBadRequest, "invalid_request_error")
		})
	}
}

func TestAgentInferenceGeoSDKNullCreatesUnset(t *testing.T) {
	s := newTestServer(t)
	model := sdk.BetaManagedAgentsModelConfigParams{ID: "custom", InferenceGeo: param.Null[string]()}
	a := createAgent(t, s, map[string]any{"name": "unset geo", "model": model})
	wantInferenceGeo(t, a, "")
	status, saved := s.do(http.MethodGet, "/v1/agents/"+a["id"].(string), nil)
	if status != http.StatusOK {
		t.Fatalf("read: %d %v", status, saved)
	}
	wantInferenceGeo(t, saved, "")
}

func TestAgentInferenceGeoPersistence(t *testing.T) {
	s := newTestServer(t)
	client := sdk.NewClient(option.WithoutEnvironmentDefaults(), option.WithBaseURL(s.url), option.WithAPIKey(testKey))
	for _, geo := range []string{"us", "global"} {
		a := createAgent(t, s, map[string]any{"name": "geo", "model": map[string]any{"id": "custom", "inference_geo": geo}})
		wantInferenceGeo(t, a, geo)
		status, got := s.do(http.MethodGet, "/v1/agents/"+a["id"].(string), nil)
		if status != http.StatusOK {
			t.Fatalf("read: %d %v", status, got)
		}
		wantInferenceGeo(t, got, geo)
		decoded, err := client.Beta.Agents.Get(context.Background(), a["id"].(string), sdk.BetaAgentGetParams{})
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Model.InferenceGeo != geo {
			t.Fatalf("SDK decoded geo = %q, want %q", decoded.Model.InferenceGeo, geo)
		}
	}
}

func TestSessionInferenceGeoOverridesAndSnapshots(t *testing.T) {
	s := newTestServer(t)
	id, env := fixture(t, s)
	status, base := s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"model": map[string]any{"id": "custom", "inference_geo": "us"}})
	if status != http.StatusOK {
		t.Fatalf("update: %d %v", status, base)
	}
	var sessions []map[string]any
	for _, tc := range []struct {
		model any
		geo   string
	}{
		{nil, "us"}, {map[string]any{"id": "custom", "inference_geo": "global"}, "global"},
		{sdk.BetaManagedAgentsModelConfigParams{ID: "custom", InferenceGeo: param.Null[string]()}, ""},
		{map[string]any{"id": "custom"}, ""}, {"custom", ""},
	} {
		agent := map[string]any{"type": "agent_with_overrides", "id": id, "version": base["version"]}
		if tc.model != nil {
			agent["model"] = tc.model
		}
		session := createSession(t, s, map[string]any{"agent": agent, "environment_id": env})
		wantInferenceGeo(t, session["agent"].(map[string]any), tc.geo)
		sessions = append(sessions, map[string]any{"id": session["id"], "geo": tc.geo})
	}
	status, got := s.do(http.MethodGet, "/v1/agents/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("base read: %d %v", status, got)
	}
	wantInferenceGeo(t, got, "us")
	status, got = s.do(http.MethodPost, "/v1/agents/"+id, map[string]any{"model": map[string]any{"id": "custom", "inference_geo": "global"}})
	if status != http.StatusOK {
		t.Fatalf("base update: %d %v", status, got)
	}
	for _, session := range sessions {
		status, got := s.do(http.MethodGet, "/v1/sessions/"+session["id"].(string), nil)
		if status != http.StatusOK {
			t.Fatalf("session read: %d %v", status, got)
		}
		wantInferenceGeo(t, got["agent"].(map[string]any), session["geo"].(string))
	}
	pinned := createSession(t, s, map[string]any{"environment_id": env,
		"agent": map[string]any{"type": "agent", "id": id, "version": base["version"]}})
	wantInferenceGeo(t, pinned["agent"].(map[string]any), "us")
}

func TestAgentInferenceGeoUpdatesAndVersions(t *testing.T) {
	s := newTestServer(t)
	a := createAgent(t, s, map[string]any{"name": "geo", "model": map[string]any{"id": "custom", "inference_geo": "us", "effort": "high"}})
	id := a["id"].(string)
	for _, tc := range []struct {
		body map[string]any
		geo  string
	}{
		{map[string]any{"description": "unrelated update"}, "us"},
		{map[string]any{"model": map[string]any{"id": "custom", "inference_geo": "global"}}, "global"},
		{map[string]any{"model": map[string]any{"id": "custom"}}, ""},
		{map[string]any{"model": map[string]any{"id": "custom", "inference_geo": "us"}}, "us"},
		{map[string]any{"model": sdk.BetaManagedAgentsModelConfigParams{ID: "custom", InferenceGeo: param.Null[string]()}}, ""},
		{map[string]any{"model": map[string]any{"id": "custom", "inference_geo": "us"}}, "us"},
		{map[string]any{"model": "custom"}, ""},
	} {
		status, got := s.do(http.MethodPost, "/v1/agents/"+id, tc.body)
		if status != http.StatusOK {
			t.Fatalf("update: %d %v", status, got)
		}
		wantInferenceGeo(t, got, tc.geo)
		wantEffort(t, got, "high")
	}
	status, pinned := s.do(http.MethodGet, "/v1/agents/"+id+"?version=1", nil)
	if status != http.StatusOK {
		t.Fatalf("version read: %d %v", status, pinned)
	}
	wantInferenceGeo(t, pinned, "us")
}

func TestInferenceGeoRosterSnapshotsAreMetadata(t *testing.T) {
	s := newTestServer(t)
	child := createAgent(t, s, map[string]any{"name": "child", "model": map[string]any{"id": "custom", "inference_geo": "global"}})
	coord := createAgent(t, s, map[string]any{"name": "coordinator", "model": map[string]any{"id": "custom", "inference_geo": "us"},
		"multiagent": map[string]any{"type": "coordinator", "agents": []any{child["id"], map[string]any{"type": "self"}}}})
	env := createEnvironment(t, s, map[string]any{"name": "geo"})
	session := createSession(t, s, map[string]any{"agent": coord["id"], "environment_id": env["id"]})
	status, saved := s.do(http.MethodGet, "/v1/sessions/"+session["id"].(string), nil)
	if status != http.StatusOK {
		t.Fatalf("read session: %d %v", status, saved)
	}
	agent := saved["agent"].(map[string]any)
	wantInferenceGeo(t, agent, "us")
	roster := rosterOf(t, agent)
	if len(roster) != 2 {
		t.Fatalf("roster = %v, want child and self", roster)
	}
	wantInferenceGeo(t, roster[0], "global")
	wantInferenceGeo(t, roster[1], "us")
}
