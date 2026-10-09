package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/provider/anthropic"
)

// endTurn is a minimal answer: one text block, then the stop.
var endTurn = []string{
	`{"type":"message_start","message":{"id":"msg_p","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"usage":{"input_tokens":5,"output_tokens":1}}}`,
	`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
	`{"type":"content_block_stop","index":0}`,
	`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
	`{"type":"message_stop"}`,
}

// generateDone runs one turn of text and returns its done chunk.
func generateDone(p provider.Provider, text string) (provider.Chunk, error) {
	stream, err := p.Generate(context.Background(), provider.Request{
		Messages: []provider.Message{{Role: "user", Content: json.RawMessage(fmt.Sprintf("%q", text))}},
	})
	if err != nil {
		return provider.Chunk{}, err
	}
	defer stream.Close()
	var last provider.Chunk
	for stream.Next() {
		last = stream.Chunk()
	}
	if err := stream.Err(); err != nil {
		return provider.Chunk{}, err
	}
	if last.Kind != provider.KindDone {
		return provider.Chunk{}, fmt.Errorf("last chunk = %+v, want done", last)
	}
	return last, nil
}

func doneOf(t *testing.T, p provider.Provider, text string) provider.Chunk {
	t.Helper()
	done, err := generateDone(p, text)
	if err != nil {
		t.Fatal(err)
	}
	return done
}

// The done chunk reports the gateway's word that the turn's thinking may go
// back under any prefix (provider.ThinkingPrefixHeader), and only that word:
// no header, or another value, reports nothing.
func TestGenerateReportsTheThinkingPrefixWord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		want   bool
	}{
		{"unchecked", http.Header{provider.ThinkingPrefixHeader: {"unchecked"}}, true},
		{"absent", nil, false},
		{"another value", http.Header{provider.ThinkingPrefixHeader: {"checked"}}, false},
	} {
		f := &fakeServer{sse: endTurn, header: tc.header}
		if got := doneOf(t, start(t, f), "hi").ThinkingAnyPrefix; got != tc.want {
			t.Errorf("%s: ThinkingAnyPrefix = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The answer that streams decides: a refused attempt the SDK retries carries
// its word away with it.
func TestTheThinkingPrefixWordIsTheStreamedAnswers(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set(provider.ThinkingPrefixHeader, "unchecked")
			w.Header().Set("x-should-retry", "true")
			w.Header().Set("retry-after-ms", "1")
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		for _, data := range endTurn {
			var m struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(data), &m)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Type, data)
		}
	}))
	t.Cleanup(srv.Close)
	p, err := anthropic.New(provider.Config{Protocol: "anthropic", Model: "m", BaseURL: srv.URL, APIKey: testAPIKey})
	if err != nil {
		t.Fatal(err)
	}
	if done := doneOf(t, p, "hi"); done.ThinkingAnyPrefix {
		t.Error("ThinkingAnyPrefix carried over from a refused attempt")
	}
	if n.Load() != 2 {
		t.Fatalf("the endpoint was called %d times, want a retry", n.Load())
	}
}

// One client serves every call, and each call hears its own answer's word.
func TestTheThinkingPrefixWordIsPerCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) == 1 && strings.HasPrefix(body.Messages[0].Content, "unchecked") {
			w.Header().Set(provider.ThinkingPrefixHeader, "unchecked")
		}
		w.Header().Set("content-type", "text/event-stream")
		for _, data := range endTurn {
			var m struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(data), &m)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Type, data)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	p, err := anthropic.New(provider.Config{Protocol: "anthropic", Model: "m", BaseURL: srv.URL, APIKey: testAPIKey})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, want := fmt.Sprintf("checked %d", i), false
			if i%2 == 0 {
				text, want = fmt.Sprintf("unchecked %d", i), true
			}
			done, err := generateDone(p, text)
			switch {
			case err != nil:
				t.Errorf("%s: %v", text, err)
			case done.ThinkingAnyPrefix != want:
				t.Errorf("%s: ThinkingAnyPrefix = %v, want %v", text, done.ThinkingAnyPrefix, want)
			}
		}()
	}
	wg.Wait()
}
