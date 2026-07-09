package domain

import (
	"encoding/json"
	"testing"
)

func TestModelUnmarshalBareString(t *testing.T) {
	var m Model
	if err := json.Unmarshal([]byte(`"claude-opus-4-8"`), &m); err != nil {
		t.Fatalf("unmarshal bare string: %v", err)
	}
	if m.ID != "claude-opus-4-8" || m.Speed != "" {
		t.Errorf("got %+v, want {ID:claude-opus-4-8}", m)
	}
}

func TestModelUnmarshalObject(t *testing.T) {
	var m Model
	if err := json.Unmarshal([]byte(`{"id":"claude-opus-4-8","speed":"fast"}`), &m); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}
	if m.ID != "claude-opus-4-8" || m.Speed != "fast" {
		t.Errorf("got %+v, want {ID:claude-opus-4-8, Speed:fast}", m)
	}
}

func TestModelMarshalsToObject(t *testing.T) {
	b, err := json.Marshal(Model{ID: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `{"id":"claude-sonnet-5"}` {
		t.Errorf("marshal = %s, want {\"id\":\"claude-sonnet-5\"}", b)
	}
}

func TestModelRoundTripInsideAgentSpec(t *testing.T) {
	in := []byte(`{"model":"claude-opus-4-8","system":"be helpful"}`)
	var spec AgentSpec
	if err := json.Unmarshal(in, &spec); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if spec.Model.ID != "claude-opus-4-8" {
		t.Errorf("spec.Model.ID = %q, want claude-opus-4-8", spec.Model.ID)
	}
	if spec.System != "be helpful" {
		t.Errorf("spec.System = %q", spec.System)
	}
}
