package upstream_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/upstream"
)

func TestReaderSplitsEvents(t *testing.T) {
	stream := "event: message_start\ndata: {\"a\":1}\n\n" +
		": keep-alive\n\n" +
		"event: ping\r\ndata: {}\r\n\r\n" +
		"event: content_block_delta\ndata: line one\ndata: line two\n\n" +
		"data:no-space\n\n" +
		"event: trailing\ndata: x\n"
	r := upstream.NewReader(strings.NewReader(stream))
	type got struct{ raw, name, data string }
	want := []got{
		{"event: message_start\ndata: {\"a\":1}\n\n", "message_start", `{"a":1}`},
		{": keep-alive\n\n", "", ""},
		{"event: ping\r\ndata: {}\r\n\r\n", "ping", "{}"},
		{"event: content_block_delta\ndata: line one\ndata: line two\n\n", "content_block_delta", "line one\nline two"},
		{"data:no-space\n\n", "", "no-space"},
	}
	for i, w := range want {
		e, err := r.Next()
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if string(e.Raw) != w.raw || e.Name != w.name || string(e.Data) != w.data {
			t.Errorf("event %d = %q %q %q, want %q %q %q", i, e.Raw, e.Name, e.Data, w.raw, w.name, w.data)
		}
	}
	e, err := r.Next()
	if !errors.Is(err, io.EOF) || string(e.Raw) != "event: trailing\ndata: x\n" || string(e.Data) != "x" {
		t.Errorf("trailing = %q %q, %v", e.Raw, e.Data, err)
	}
	if e.Data == nil {
		t.Error("trailing data lost")
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the end: %v", err)
	}
}

func TestReaderKeepsALongLineWhole(t *testing.T) {
	long := strings.Repeat("x", 200_000)
	r := upstream.NewReader(strings.NewReader("event: big\ndata: " + long + "\n\n"))
	e, err := r.Next()
	if err != nil || string(e.Data) != long || e.Name != "big" {
		t.Fatalf("long line: name %q, %d bytes, %v", e.Name, len(e.Data), err)
	}
}

func TestReaderBoundsAnEvent(t *testing.T) {
	huge := strings.Repeat("data: "+strings.Repeat("y", 1<<20)+"\n", 17)
	if _, err := upstream.NewReader(strings.NewReader(huge)).Next(); !errors.Is(err, upstream.ErrEventTooLarge) {
		t.Errorf("an event of 17 MiB: %v", err)
	}
	if _, err := upstream.NewReader(strings.NewReader("data: " + strings.Repeat("z", 17<<20))).Next(); !errors.Is(err, upstream.ErrEventTooLarge) {
		t.Errorf("one line of 17 MiB: %v", err)
	}
}

func TestWithDataReplacesTheDataLines(t *testing.T) {
	e := upstream.Event{Raw: []byte("event: message_start\nid: 7\ndata: one\ndata: two\n\n")}
	got := string(e.WithData([]byte(`{"b":2}`)))
	want := "event: message_start\nid: 7\ndata: {\"b\":2}\n\n"
	if got != want {
		t.Errorf("WithData = %q, want %q", got, want)
	}
}

// The client hands a redirect back as the response rather than following it,
// so a credential never reaches the redirect's target.
func TestClientFollowsNoRedirect(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	req, _ := http.NewRequest(http.MethodPost, origin.URL, strings.NewReader("{}"))
	req.Header.Set("x-api-key", "sk-secret")
	resp, err := upstream.NewClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || reached {
		t.Errorf("status %d, target reached %t", resp.StatusCode, reached)
	}
}
