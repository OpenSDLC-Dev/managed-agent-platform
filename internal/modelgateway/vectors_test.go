package modelgateway_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
)

// answers are the bodies the rewriter is held to: Gitee's three answer
// shapes, and the JSON an answer may take that a rewriter reading it a
// piece at a time could misread.
var answers = []string{
	`{"object":"list","data":[{"object":"embedding","embedding":[0.5,1.7366719305164412E-5,-2],"index":0}],"model":"Qwen3-Embedding-8B","usage":{"prompt_tokens":13,"total_tokens":13}}`,
	`{"object":"list","data":[{"object":"embedding","embedding":"AAAAPwAAgD8AAADA","index":0}],"model":"Qwen3-Embedding-8B","usage":{"prompt_tokens":13,"total_tokens":13}}`,
	`{"model":"bge-reranker-v2-m3","usage":{"totalTokens":0,"promptTokens":0},"results":[{"index":1,"document":{"text":"Jupiter is the <i>largest</i> planet."},"relevance_score":0.99}]}`,
	` { "usage" : { "prompt_tokens" : 1 } , "model" : "a" , "data" : [ ] } `,
	"{\n\t\"model\":\r\n\"a\"}\n",
	`{"data":{"model":"nested","usage":{"prompt_tokens":99}},"model":"top"}`,
	`{"mod\u0065l":"escaped","usage":{"prompt_tokens":2}}`,
	`{"model\"":"not model","models":"nor this","Model":"nor this"}`,
	`{"k\"":"v","model":"m","x\\":1,"usage":{"prompt_tokens":4}}`,
	`{"model":1.5e3,"usage":null,"x":true,"y":false,"z":null}`,
	`{"model": 7 ,"usage": 3 ,"n":-0.5e-3 }`,
	`{"model":{"id":"m","alias":["a","}"]},"usage":[1,{"a":"]"}]}`,
	`{"model":"a","model":"b","usage":{"prompt_tokens":1},"usage":{"prompt_tokens":2}}`,
	`{"s":"{\"model\":\"in a string\"}\\","model":"m\"\\\u00e9","t":"日本語"}`,
	`{"usage":"` + strings.Repeat("x", modelgateway.MaxAnswerUsage) + `"}`,
	`{"usage":{"prompt_tokens":5},"usage":"` + strings.Repeat("x", modelgateway.MaxAnswerUsage) + `"}`,
	`{"data":[],"usage":{` + strings.Repeat(" ", 70000) + `"prompt_tokens" : 13 , "s":" a b "}}`,
	// A usage at the bound and one past it, either padded past it with
	// whitespace between its tokens: the bound counts the usage without it.
	`{"usage":{"prompt_tokens":13,"s":"` + strings.Repeat("x", modelgateway.MaxAnswerUsage-27) + `"` + strings.Repeat(" ", 70000) + `}}`,
	`{"usage":{"prompt_tokens":13,"s":"` + strings.Repeat("x", modelgateway.MaxAnswerUsage-26) + `"` + strings.Repeat(" ", 70000) + `}}`,
	`{}`, `{ }`, `[{"model":"in an array"}]`, `"model"`, `null`, `12`,
	`{"model":`, `{"model":"cut`, `{"usage":{"prompt_tokens":1`, `{"a":1,}`, `{"a" 1,"model":"m"}`, `}{"model":"m"}`, ``,
}

// rewritten is what the rewriter should make of in, a JSON object: each
// top-level model value replaced by alias and every other byte kept, with
// the last top-level usage value, compacted — found by encoding/json's
// decoder, which shares none of the rewriter's code. It reports false for
// anything but an object.
func rewritten(t testing.TB, in []byte, alias string) ([]byte, json.RawMessage, bool) {
	t.Helper()
	if !json.Valid(in) || bytes.TrimLeft(in, " \t\r\n")[0] != '{' {
		return nil, nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(in))
	_, _ = dec.Token()
	var out []byte
	var usage json.RawMessage
	last := 0
	for dec.More() {
		key, _ := dec.Token()
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			t.Fatal(err)
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		if !bytes.Equal(in[start:end], raw) {
			t.Fatalf("the oracle misplaced %s in %s", raw, in)
		}
		switch key {
		case "model":
			out = append(append(out, in[last:start]...), `"`+alias+`"`...)
			last = end
		case "usage":
			var c bytes.Buffer
			_ = json.Compact(&c, raw)
			usage = c.Bytes()
			if c.Len() > modelgateway.MaxAnswerUsage {
				usage = nil
			}
		}
	}
	return append(out, in[last:]...), usage, true
}

// compacted is a kept usage as encoding/json compacts it, nil for none.
func compacted(usage json.RawMessage) json.RawMessage {
	if usage == nil {
		return nil
	}
	var c bytes.Buffer
	if json.Compact(&c, usage) != nil {
		return usage
	}
	return c.Bytes()
}

// The rewriter makes of an answer what the decoder says it should, however
// the answer is cut into pieces: whole, in two at every byte, and a byte at
// a time. What is not an object comes back as it went in.
func TestAnswerRewriter(t *testing.T) {
	for _, a := range answers {
		in := []byte(a)
		whole, usage := modelgateway.RewriteAnswer("al\"ias", in)
		want, wantUsage, object := rewritten(t, in, `al\"ias`)
		switch {
		case object && (!bytes.Equal(whole, want) || !bytes.Equal(compacted(usage), wantUsage)):
			t.Errorf("%.80s: rewrote %.200s and kept %.80s, want %.200s and %.80s", a, whole, usage, want, wantUsage)
		case json.Valid(in) && !object && (!bytes.Equal(whole, in) || usage != nil):
			t.Errorf("%.80s: rewrote %.200s and kept %.80s", a, whole, usage)
		}
		step := 1
		if len(in) > 1<<10 { // a usage past the bound: 64 cuts are plenty
			step = len(in)/64 + 1
		}
		for i := 0; i < len(in); i += step {
			if got, u := modelgateway.RewriteAnswer("al\"ias", in[:i], in[i:]); !bytes.Equal(got, whole) || !bytes.Equal(u, usage) {
				t.Errorf("%.80s cut at %d: rewrote %.200s", a, i, got)
			}
		}
		var bytewise [][]byte
		for i := range in {
			bytewise = append(bytewise, in[i:i+1])
		}
		if got, u := modelgateway.RewriteAnswer("al\"ias", bytewise...); !bytes.Equal(got, whole) || !bytes.Equal(u, usage) {
			t.Errorf("%.80s a byte at a time: rewrote %.200s", a, got)
		}
	}
}

// FuzzAnswerRewriter holds the rewriter to the decoder on any object, and
// to its own whole reading on any input cut anywhere.
func FuzzAnswerRewriter(f *testing.F) {
	for _, a := range answers {
		f.Add([]byte(a), uint16(len(a)/2))
	}
	f.Fuzz(func(t *testing.T, in []byte, cut uint16) {
		whole, usage := modelgateway.RewriteAnswer("m", in)
		i := int(cut) % (len(in) + 1)
		if got, u := modelgateway.RewriteAnswer("m", in[:i], in[i:]); !bytes.Equal(got, whole) || !bytes.Equal(u, usage) {
			t.Fatalf("cut at %d: %q, whole %q", i, got, whole)
		}
		want, wantUsage, object := rewritten(t, in, "m")
		switch {
		case object && (!bytes.Equal(whole, want) || !bytes.Equal(compacted(usage), wantUsage)):
			t.Fatalf("rewrote %q and kept %q, want %q and %q", whole, usage, want, wantUsage)
		case json.Valid(in) && !object && (!bytes.Equal(whole, in) || usage != nil):
			t.Fatalf("rewrote %q and kept %q", whole, usage)
		}
	})
}

// What the rewriter holds is bounded by the piece it is given, however long
// the alias and however many model members an answer repeats: each alias it
// hands on is the one it was made with, not a copy.
func TestRewritingRepeatedModelsHoldsLittle(t *testing.T) {
	alias := strings.Repeat("x", 256<<10)
	chunk := []byte("{" + strings.Repeat(`"model":0,`, 1024) + `"n":1}`)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	n := modelgateway.RewrittenLength(alias, chunk)
	runtime.ReadMemStats(&after)
	if want := len(chunk) - 1024 + 1024*(len(alias)+2); n != want {
		t.Errorf("rewrote %d bytes, want %d", n, want)
	}
	if held := after.TotalAlloc - before.TotalAlloc; held > 8<<20 {
		t.Errorf("rewriting %d bytes allocated %d", len(chunk), held)
	}
}

// The usage the rewriter keeps reads as the upstream wrote it: padding
// between its tokens does not push it past the bound, and tokens whitespace
// parted are not joined into a count the upstream never reported.
func TestTheKeptUsageReadsAsWritten(t *testing.T) {
	for answer, want := range map[string]*store.Tokens{
		`{"data":[],"usage":{"prompt_tokens":1 3}}`:                                                nil,
		`{"data":[],"usage":{"total_tokens":4` + "\n" + `2}}`:                                      nil,
		`{"data":[],"usage":{` + strings.Repeat(" ", 70000) + `"prompt_tokens" :` + "\t" + `13 }}`: {Input: 13},
	} {
		_, usage := modelgateway.RewriteAnswer("m", []byte(answer))
		if got := modelgateway.VectorUsageOf(usage); !reflect.DeepEqual(got, want) {
			t.Errorf("%.60s: %+v, want %+v", answer, got, want)
		}
	}
}

// An embeddings or rerank usage counts its prompt tokens, or else its total,
// all of them input; nothing else is read for it.
func TestVectorUsage(t *testing.T) {
	for _, c := range []struct {
		usage string
		want  *store.Tokens
	}{
		{`{"prompt_tokens":13,"total_tokens":13}`, &store.Tokens{Input: 13}},
		{`{"total_tokens":7}`, &store.Tokens{Input: 7}},
		{`{"prompt_tokens":3,"total_tokens":7}`, &store.Tokens{Input: 3}},
		{`{"prompt_tokens":null,"total_tokens":7}`, &store.Tokens{Input: 7}},
		{`{"prompt_tokens":0}`, &store.Tokens{}},
		{`{"totalTokens":0,"promptTokens":0}`, nil},
		{`{"prompt_tokens":-1}`, nil},
		{`{"prompt_tokens":1.5}`, nil},
		{`{"input_tokens":4}`, nil},
		{`null`, nil},
		{`7`, nil},
		{``, nil},
	} {
		if got := modelgateway.VectorUsageOf([]byte(c.usage)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %+v, want %+v", c.usage, got, c.want)
		}
	}
}
