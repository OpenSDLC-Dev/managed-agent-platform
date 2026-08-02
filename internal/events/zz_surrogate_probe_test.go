package events

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProbeLoneSurrogateRubric(t *testing.T) {
	raw := json.RawMessage(`{"type":"user.define_outcome","description":"d","rubric":{"type":"text","content":"\ud800"}}`)
	evs, err := NormalizeInbound("cloud", []json.RawMessage{raw})
	if err != nil {
		t.Fatalf("normalize rejected it (good): %v", err)
	}
	p := string(evs[0].Payload)
	t.Logf("payload = %s", p)
	if strings.Contains(p, `\ud800`) {
		t.Logf("RAW SURROGATE ESCAPE SURVIVES INTO PAYLOAD")
	} else {
		t.Logf("laundered")
	}
	// control: description
	raw2 := json.RawMessage(`{"type":"user.define_outcome","description":"\ud800","rubric":{"type":"text","content":"ok"}}`)
	evs2, err := NormalizeInbound("cloud", []json.RawMessage{raw2})
	if err != nil {
		t.Fatalf("desc: %v", err)
	}
	t.Logf("desc payload = %s", string(evs2[0].Payload))
	// control: user.message text block
	raw3 := json.RawMessage(`{"type":"user.message","content":[{"type":"text","text":"\ud800"}]}`)
	evs3, err := NormalizeInbound("cloud", []json.RawMessage{raw3})
	if err != nil {
		t.Fatalf("msg: %v", err)
	}
	t.Logf("msg payload = %s", string(evs3[0].Payload))
}
