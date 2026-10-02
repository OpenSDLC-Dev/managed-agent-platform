package domain

import (
	"encoding/json"
	"errors"
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

func TestModelEffortRoundTrip(t *testing.T) {
	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		for _, effort := range []string{`"` + level + `"`, `{"type":"` + level + `"}`} {
			t.Run(effort, func(t *testing.T) {
				var m Model
				if err := json.Unmarshal([]byte(`{"id":"custom","effort":`+effort+`}`), &m); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				want := `{"id":"custom","effort":{"type":"` + level + `"}}`
				if string(raw) != want {
					t.Fatalf("round trip = %s, want %s", raw, want)
				}
			})
		}
	}
}

func TestModelDecodeReplacesPreviousFields(t *testing.T) {
	for _, raw := range []string{`"other"`, `{"id":"other"}`} {
		m := Model{ID: "original", Speed: "fast", Effort: "high", InferenceGeo: "us"}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if m != (Model{ID: "other"}) {
			t.Fatalf("decode retained old fields: %+v", m)
		}
	}
}

func TestModelInferenceGeoNullClearsPreviousValue(t *testing.T) {
	geo := ModelInferenceGeo("us")
	if err := json.Unmarshal([]byte(`null`), &geo); err != nil {
		t.Fatal(err)
	}
	if geo != "" {
		t.Fatalf("null retained geo = %q", geo)
	}
}

// The agent routes answer a bad effort level string and a bad inference_geo
// string in the reference's recorded words (internal/api parseAgentModel,
// #540), and find them by these errors. Every other malformed value — the
// {type: level} form's bad level, a null, a non-string — stays a plain error,
// the reference's answer to it being unrecorded.
func TestModelRefusalsTheAgentRoutesRecognize(t *testing.T) {
	decode := func(model string) error {
		var m Model
		return json.Unmarshal([]byte(model), &m)
	}
	var effort *EffortLevelError
	if err := decode(`{"id":"custom","effort":"bogus"}`); !errors.As(err, &effort) || effort.Level != "bogus" {
		t.Errorf("bad effort string: error %v, want an *EffortLevelError carrying \"bogus\"", err)
	}
	for _, raw := range []string{`{"type":"bogus"}`, `null`, `1`, `{}`} {
		if err := decode(`{"id":"custom","effort":` + raw + `}`); err == nil || errors.As(err, &effort) {
			t.Errorf("effort %s: error %v, want a plain refusal", raw, err)
		}
	}
	for _, raw := range []string{`"bogus"`, `""`} {
		if err := decode(`{"id":"custom","inference_geo":` + raw + `}`); !errors.Is(err, ErrInferenceGeoValue) {
			t.Errorf("inference_geo %s: error %v, want ErrInferenceGeoValue", raw, err)
		}
	}
	if err := decode(`{"id":"custom","inference_geo":1}`); err == nil || errors.Is(err, ErrInferenceGeoValue) {
		t.Errorf("inference_geo 1: error %v, want a plain refusal", err)
	}
}
