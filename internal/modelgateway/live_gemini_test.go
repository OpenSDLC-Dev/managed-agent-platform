package modelgateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/convert"
)

// TestLiveGeminiCountsTheConvertedRequest has the Gemini API count the
// generateContent request convert.GeminiRequest makes of each Messages
// request below. countTokens takes a whole generateContentRequest and parses
// it as generateContent does, at no cost, so each shape the conversion
// sends is checked against Gemini's own parser while the key this tier runs
// under cannot generate (docs/plan/62_gemini-upstream-protocol.md,
// Verification; the generation rows wait on #903): a system prompt in two
// blocks; tools whose schemas carry $defs, type unions and
// additionalProperties, and a tool_choice naming one; a tool loop's history,
// its calls under their ids, the first carrying a Gemini signature and the
// second the sentinel, its results an error with an image and a
// search_result; an image in a user turn; the sampling fields; and each
// thinking setting.
func TestLiveGeminiCountsTheConvertedRequest(t *testing.T) {
	v := namedVendor(t, "gemini")
	const model = "gemini-3.8-flash"
	image := strings.TrimPrefix(redSquare(), "data:image/png;base64,")
	const tools = `"tools":[{"name":"lookup","description":"Look a record up","input_schema":{"type":"object",
		"$defs":{"id":{"type":"string","pattern":"^[a-z]+$"}},
		"properties":{"id":{"$ref":"#/$defs/id"},"limit":{"type":["integer","null"]},"tags":{"type":"array","items":{"type":"string"}}},
		"required":["id"],"additionalProperties":false}}]`
	hi := `"messages":[{"role":"user","content":"Find record abc."}]`
	loop := `"messages":[{"role":"user","content":"Find records abc and def."},
		{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"gemini:c2lnbmF0dXJl"},{"type":"text","text":"Looking."},
			{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"id":"abc"}},{"type":"tool_use","id":"toolu_2","name":"lookup","input":{"id":"def","limit":null}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"search_result","source":"https://records.example/abc",
				"title":"Record abc","content":[{"type":"text","text":"abc is a red square."}]}]},
			{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":[{"type":"text","text":"def is missing; here is its last scan"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + image + `"}}]}]}]`
	for name, body := range map[string]string{
		"system, tools and a choice": `{"max_tokens":256,"system":[{"type":"text","text":"Answer briefly."},{"type":"text","text":"Use the tool."}],` +
			hi + `,` + tools + `,"tool_choice":{"type":"tool","name":"lookup"}}`,
		"a tool loop": `{"max_tokens":256,` + loop + `,` + tools + `}`,
		"an image": `{"max_tokens":256,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` +
			image + `"}},{"type":"text","text":"What colour is this?"}]}]}`,
		"sampling":                  `{"max_tokens":64,` + hi + `,"temperature":0.4,"top_p":0.9,"top_k":20,"stop_sequences":["END"]}`,
		"thinking enabled":          `{"max_tokens":2048,` + hi + `,"thinking":{"type":"enabled","budget_tokens":1024}}`,
		"thinking enabled, omitted": `{"max_tokens":2048,` + hi + `,"thinking":{"type":"enabled","budget_tokens":1024,"display":"omitted"}}`,
		"thinking adaptive, low":    `{"max_tokens":2048,` + hi + `,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`,
		"effort medium":             `{"max_tokens":2048,` + hi + `,"output_config":{"effort":"medium"}}`,
		"effort max":                `{"max_tokens":2048,` + hi + `,"output_config":{"effort":"max"}}`,
		"thinking disabled":         `{"max_tokens":256,` + hi + `,"thinking":{"type":"disabled"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var top map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &top); err != nil {
				t.Fatalf("%s: %v", body, err)
			}
			converted, err := convert.GeminiRequest(top)
			if err != nil {
				t.Fatalf("GeminiRequest: %v", err)
			}
			payload := `{"generateContentRequest":{"model":"models/` + model + `",` + string(converted[1:]) + `}`
			req, err := http.NewRequestWithContext(liveCtx(t), http.MethodPost, v.base+"/models/"+model+":countTokens", strings.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-goog-api-key", v.keyEnv)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				liveFatalf(t, "countTokens: %v", err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			var count struct {
				TotalTokens int `json:"totalTokens"`
			}
			if resp.StatusCode != http.StatusOK || json.Unmarshal(b, &count) != nil || count.TotalTokens <= 0 {
				liveFatalf(t, "countTokens answered %d %s\nfor %s", resp.StatusCode, b, bytes.TrimSpace(converted))
			}
			t.Logf("%d tokens", count.TotalTokens)
		})
	}
}
