package modelgateway_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/profile"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/openai/openai-go/v3"
	oaoption "github.com/openai/openai-go/v3/option"
)

// TestLiveGiteeEmbeddingsAndRerank drives Gitee AI's embeddings and rerank
// through the gateway, its provider behind a recording proxy:
//   - text embeddings through openai-go with dimensions, asked as float and
//     as base64, decode to vectors of that length that agree, and the
//     encoding Gitee answers a base64 request in is logged;
//   - a multimodal request, a text and an image, is answered with vectors;
//   - rerank, through a plain HTTP client since no SDK covers it, ranks the
//     document that answers the query first, its scores mapped back by
//     index;
//   - each call's cap refuses one input past it with a 400 relayed in
//     Gitee's own words, and text embeddings and rerank take as many as
//     the cap — a multimodal batch that large costs Gitee's resource
//     package tens of thousands of tokens a run;
//
// every answer the vendor's but for model, and the ledger counting the usage
// the vendor reported in OpenAI's keys, or none.
func TestLiveGiteeEmbeddingsAndRerank(t *testing.T) {
	v := namedVendor(t, "gitee")
	e := newEnv(t)
	rec := &recorder{}
	proxy := recordingProxy(t, v.base, rec)
	p := e.provider(proxy, func(p *store.Provider) {
		p.Name, p.Profile, p.Endpoints = "gitee", "gitee", map[profile.Protocol]string{profile.OpenAI: proxy}
	})
	e.credential(p, v.keyEnv, 1)
	embedding := func(d *store.Deployment) { d.Kind = store.KindEmbedding }
	e.alias("text", target(e.deployment(p, "Qwen3-Embedding-8B", embedding), 0))
	// Qwen3-VL-Embedding-8B, dikw-core's choice, is outside the resource
	// package of the key this tier ran under on 2026-10-08.
	e.alias("vision", target(e.deployment(p, "Qwen3-VL-Embedding-2B", embedding), 0))
	e.alias("ranker", target(e.deployment(p, "bge-reranker-v2-m3", func(d *store.Deployment) { d.Kind = store.KindRerank }), 0))
	key := e.key(everyAlias)
	e.start()
	bearer := map[string]string{"Authorization": "Bearer " + key, "Content-Type": "application/json"}

	t.Run("text", func(t *testing.T) {
		const dims = 512
		cl := e.oaClient(key, "/v1")
		in := []string{"Jupiter is the largest planet in the solar system.", "Octopuses have three hearts."}
		got := map[openai.EmbeddingNewParamsEncodingFormat][][]float64{}
		for _, format := range []openai.EmbeddingNewParamsEncodingFormat{openai.EmbeddingNewParamsEncodingFormatFloat, openai.EmbeddingNewParamsEncodingFormatBase64} {
			var resp *http.Response
			n := rec.count()
			res, err := cl.Embeddings.New(liveCtx(t), openai.EmbeddingNewParams{Model: "text", Dimensions: openai.Int(dims), EncodingFormat: format,
				Input: openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: in}}, oaoption.WithResponseInto(&resp))
			if err != nil {
				liveFatalf(t, "%s: %v", format, err)
			}
			_, raw := one(t, rec, n).answer()
			relayed(t, raw, []byte(res.RawJSON()), "text")
			vecs := make([][]float64, len(in))
			for _, d := range res.Data {
				vec, encoding, err := embeddingOf(d.JSON.Embedding.Raw())
				if err != nil || d.Index < 0 || d.Index >= int64(len(in)) || len(vec) != dims {
					liveFatalf(t, "%s: vector %d of %d values, %v", format, d.Index, len(vec), err)
				}
				if format == openai.EmbeddingNewParamsEncodingFormatBase64 && d.Index == 0 {
					t.Logf("Gitee answers a base64 request with %s vectors", encoding)
				}
				vecs[d.Index] = vec
			}
			if res.Model != "text" || len(res.Data) != len(in) {
				liveFatalf(t, "%s: answered %q with %d vectors", format, res.Model, len(res.Data))
			}
			got[format] = vecs
			liveUsageLedger(t, e, resp, raw, "embeddings")
		}
		for i, f := range got[openai.EmbeddingNewParamsEncodingFormatFloat] {
			for j, x := range f {
				// A base64 vector is float32; a float one as Gitee rounds it.
				if y := got[openai.EmbeddingNewParamsEncodingFormatBase64][i][j]; math.Abs(x-y) > 1e-6 {
					liveFatalf(t, "vector %d differs at %d: %g as float, %g as base64", i, j, x, y)
				}
			}
		}
	})

	t.Run("multimodal", func(t *testing.T) {
		body := fmt.Sprintf(`{"model":"vision","input":[{"text":"a red square"},{"image":%q}]}`, redSquare())
		n := rec.count()
		resp, b := e.do("POST", "/v1/embeddings", body, bearer)
		_, raw := one(t, rec, n).answer()
		if resp.StatusCode != 200 {
			liveFatalf(t, "%d %s", resp.StatusCode, b)
		}
		relayed(t, raw, b, "vision")
		// How many vectors is Gitee's to say: Qwen3-VL-Embedding-2B answers
		// one for a request holding an image (probed 2026-10-08).
		var res openai.CreateEmbeddingResponse
		if err := json.Unmarshal(b, &res); err != nil || len(res.Data) == 0 || len(res.Data[0].Embedding) == 0 {
			liveFatalf(t, "answered %s (%v)", abbreviated(b), err)
		}
		t.Logf("a text and an image: %d vectors of %d values, usage %s", len(res.Data), len(res.Data[0].Embedding), res.Usage.RawJSON())
		liveUsageLedger(t, e, resp, raw, "embeddings")
	})

	t.Run("rerank", func(t *testing.T) {
		docs := []string{"Octopuses have three hearts.", "Jupiter is the largest planet in the solar system.", "Paris is the capital of France."}
		body, _ := json.Marshal(map[string]any{"model": "ranker", "query": "Which planet is the largest?", "documents": docs, "top_n": len(docs)})
		n := rec.count()
		resp, b := e.do("POST", "/v1/rerank", string(body), bearer)
		_, raw := one(t, rec, n).answer()
		if resp.StatusCode != 200 {
			liveFatalf(t, "%d %s", resp.StatusCode, b)
		}
		relayed(t, raw, b, "ranker")
		var res struct {
			Results []struct {
				Index          int     `json:"index"`
				RelevanceScore float64 `json:"relevance_score"`
			} `json:"results"`
		}
		scores := make([]float64, len(docs))
		seen := map[int]bool{}
		_ = json.Unmarshal(b, &res)
		for _, r := range res.Results {
			if r.Index < 0 || r.Index >= len(docs) || seen[r.Index] {
				liveFatalf(t, "result index %d in %s", r.Index, b)
			}
			seen[r.Index], scores[r.Index] = true, r.RelevanceScore
		}
		if len(res.Results) != len(docs) || scores[1] <= scores[0] || scores[1] <= scores[2] {
			liveFatalf(t, "scores %v by document for %s", scores, b)
		}
		liveUsageLedger(t, e, resp, raw, "rerank")
	})

	t.Run("batch caps", func(t *testing.T) {
		list := func(n int, item func(int) string) string {
			items := make([]string, n)
			for i := range items {
				items[i] = item(i)
			}
			return "[" + strings.Join(items, ",") + "]"
		}
		for _, c := range []struct {
			name, path, field string
			cap               int
			atCap             bool
			body              func(n int) string
		}{
			{"text", "/v1/embeddings", "input", 1000, true, func(n int) string {
				return `{"model":"text","dimensions":256,"input":` + list(n, func(i int) string { return strconv.Quote(fmt.Sprintf("t%d", i)) }) + `}`
			}},
			{"multimodal", "/v1/embeddings", "input", 1000, false, func(n int) string {
				return `{"model":"vision","input":` + list(n, func(i int) string { return fmt.Sprintf(`{"text":"t%d"}`, i) }) + `}`
			}},
			{"rerank", "/v1/rerank", "documents", 25, true, func(n int) string {
				return fmt.Sprintf(`{"model":"ranker","query":"q","top_n":%d,"documents":%s}`, n, list(n, func(i int) string { return strconv.Quote(fmt.Sprintf("d%d", i)) }))
			}},
		} {
			if c.atCap {
				start := time.Now()
				resp, b := e.do("POST", c.path, c.body(c.cap), bearer)
				var at struct {
					Data    []json.RawMessage `json:"data"`
					Results []json.RawMessage `json:"results"`
				}
				if _ = json.Unmarshal(b, &at); resp.StatusCode != 200 || len(at.Data)+len(at.Results) != c.cap {
					liveFatalf(t, "%s: %d inputs answered %d: %s", c.name, c.cap, resp.StatusCode, abbreviated(b))
				}
				t.Logf("%s: %d inputs answered in %s, %d bytes", c.name, c.cap, time.Since(start).Round(time.Millisecond), len(b))
			}
			n := rec.count()
			resp, b := e.do("POST", c.path, c.body(c.cap+1), bearer)
			status, raw := one(t, rec, n).answer()
			if resp.StatusCode != 400 || status != 400 || !reflect.DeepEqual(jsonValue(b), jsonValue(raw)) || !strings.Contains(string(b), c.field) {
				liveFatalf(t, "%s: %d inputs answered %d %s; Gitee sent %d %s", c.name, c.cap+1, resp.StatusCode, b, status, raw)
			}
			t.Logf("%s: past %d inputs Gitee answers %s", c.name, c.cap, b)
		}
	})
}

// namedVendor is the vendor of that name when it is consented to; a test
// of it skips otherwise.
func namedVendor(t *testing.T, name string) liveVendor {
	t.Helper()
	for _, v := range namedVendors(t) {
		if v.name == name {
			return v
		}
	}
	t.Skipf("%s does not name %s", liveEnv, name)
	return liveVendor{}
}

// relayed fails unless the gateway's answer is the vendor's, but for model,
// which names the alias: each other member byte for byte, vectors included.
func relayed(t *testing.T, vendor, gateway []byte, alias string) {
	t.Helper()
	var up, got map[string]json.RawMessage
	if json.Unmarshal(vendor, &up) != nil || json.Unmarshal(gateway, &got) != nil || len(up) != len(got) || string(got["model"]) != strconv.Quote(alias) {
		liveFatalf(t, "the gateway answered %s; the vendor sent %s", abbreviated(gateway), abbreviated(vendor))
	}
	for k, v := range up {
		if k != "model" && !bytes.Equal(got[k], v) {
			liveFatalf(t, "the gateway answered %s = %s; the vendor sent %s", k, abbreviated(got[k]), abbreviated(v))
		}
	}
}

// embeddingOf decodes one embedding as an OpenAI client does: an array of
// floats, or base64 of little-endian float32s. It names which it was.
func embeddingOf(raw string) ([]float64, string, error) {
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err == nil {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(b)%4 != 0 {
			return nil, "base64", fmt.Errorf("not base64 float32s: %v", err)
		}
		out := make([]float64, len(b)/4)
		for i := range out {
			out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:])))
		}
		return out, "base64", nil
	}
	var out []float64
	err := json.Unmarshal([]byte(raw), &out)
	return out, "float", err
}

// liveUsageLedger fails unless resp's ledger row is on endpoint with the
// tokens the vendor's usage reported in OpenAI's keys, or none where it
// reported none in them.
func liveUsageLedger(t *testing.T, e *env, resp *http.Response, vendor []byte, endpoint string) {
	t.Helper()
	var want *store.Tokens
	var u struct {
		Usage struct {
			PromptTokens *int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(vendor, &u) == nil && u.Usage.PromptTokens != nil {
		want = &store.Tokens{Input: *u.Usage.PromptTokens}
	}
	rid := resp.Header.Get("request-id")
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		for _, row := range e.ledger() {
			if row.RequestID == rid {
				if row.Protocol != "openai" || row.Endpoint != endpoint || row.Status != 200 || !reflect.DeepEqual(row.Tokens, want) {
					liveFatalf(t, "ledger %s: %+v %+v, want %s and %+v", rid, row, row.Tokens, endpoint, want)
				}
				return
			}
		}
	}
	liveFatalf(t, "no ledger row for %q", rid)
}

// redSquare is a small PNG as a data URL, the shape dikw-core sends an
// image in.
func redSquare() string {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for x := range 32 {
		for y := range 32 {
			img.Set(x, y, color.RGBA{R: 220, G: 20, B: 20, A: 255})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}

// jsonValue is b decoded, for comparing two encodings of one value.
func jsonValue(b []byte) any {
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}

// abbreviated keeps a failure readable: an answer runs to megabytes of
// vectors.
func abbreviated(b []byte) string {
	if s := masked(string(b)); len(s) > 600 {
		return fmt.Sprintf("%s…(%d bytes)", s[:600], len(s))
	}
	return masked(string(b))
}
