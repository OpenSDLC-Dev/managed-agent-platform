package upstream_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// A data line with nothing on it is data still, which a client dispatches;
// a block of comments alone has none.
func TestReaderKeepsAnEmptyDataLine(t *testing.T) {
	r := upstream.NewReader(strings.NewReader("data:\n\n: keep-alive\n\ndata:"))
	for i, data := range []bool{true, false, true} {
		e, err := r.Next()
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("event %d: %v", i, err)
		}
		if (e.Data != nil) != data || len(e.Data) != 0 {
			t.Errorf("event %d: data %q, nil %v", i, e.Data, e.Data == nil)
		}
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

// roundTripper is a transport that is not an *http.Transport.
type roundTripper struct{}

func (roundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("unused")
}

// NoReuse turns keep-alives off on a clone of the client's transport, the
// default one included, leaving the original's as it was; a transport of
// another kind is the caller's, and comes back as it is.
func TestNoReuseKeepsNoConnection(t *testing.T) {
	c := upstream.NewClient()
	fresh := upstream.NoReuse(c)
	if ft, ok := fresh.Transport.(*http.Transport); !ok || !ft.DisableKeepAlives || fresh.CheckRedirect == nil {
		t.Errorf("NoReuse(NewClient()) = %+v", fresh)
	}
	if c.Transport.(*http.Transport).DisableKeepAlives {
		t.Error("NoReuse changed the client it was given")
	}
	if ft, ok := upstream.NoReuse(&http.Client{}).Transport.(*http.Transport); !ok || !ft.DisableKeepAlives || ft == http.DefaultTransport {
		t.Error("NoReuse left the default transport keeping connections")
	}
	custom := &http.Client{Transport: roundTripper{}}
	if upstream.NoReuse(custom) != custom {
		t.Error("NoReuse replaced a transport of another kind")
	}
}

// Over HTTP/2, as over HTTP/1.1, NoReuse's requests each go on a connection
// of their own, and none on the one its client keeps for others.
func TestNoReuseKeepsNoConnectionOverHTTP2(t *testing.T) {
	var mu sync.Mutex
	var remotes []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes = append(remotes, fmt.Sprintf("%d %s", r.ProtoMajor, r.RemoteAddr))
		mu.Unlock()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	shared := upstream.NewClient()
	shared.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	fresh := upstream.NoReuse(shared)
	for range 2 {
		for _, c := range []*http.Client{shared, fresh} {
			resp, err := c.Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]int{}
	for _, r := range remotes {
		seen[r]++
	}
	// The shared client's two requests share a connection; each of the
	// fresh client's has one to itself.
	if len(remotes) != 4 || len(seen) != 3 || seen[remotes[0]] != 2 || !strings.HasPrefix(remotes[0], "2 ") {
		t.Errorf("4 requests on connections %v", remotes)
	}
}
