package gate

import (
	"net/http"
	"testing"
	"time"
)

func TestDefaultTransportConfig(t *testing.T) {
	g := New(Config{})
	tr, ok := g.transport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T, want *http.Transport", g.transport)
	}
	// A transparent forward proxy must not negotiate/undo compression itself, and
	// must bound a stalled origin's time-to-response-headers.
	if !tr.DisableCompression {
		t.Error("default transport must set DisableCompression so responses forward verbatim")
	}
	if tr.ResponseHeaderTimeout != 60*time.Second {
		t.Errorf("default ResponseHeaderTimeout = %v, want 60s", tr.ResponseHeaderTimeout)
	}
}

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"example.com:443": "example.com",
		"example.com":     "example.com", // no port
		"10.1.2.3:8080":   "10.1.2.3",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAddrWithPort(t *testing.T) {
	if got := addrWithPort("example.com", "443"); got != "example.com:443" {
		t.Errorf("addrWithPort default port = %q", got)
	}
	if got := addrWithPort("example.com:8443", "443"); got != "example.com:8443" {
		t.Errorf("addrWithPort existing port = %q, want it kept", got)
	}
}

func TestRemoveHopByHop(t *testing.T) {
	h := http.Header{}
	h.Set("Connection", "X-Custom, Keep-Alive")
	h.Set("X-Custom", "drop-me")      // named in Connection → hop-by-hop for this hop
	h.Set("Proxy-Connection", "1")    // always hop-by-hop
	h.Set("Authorization", "keep-me") // end-to-end
	removeHopByHop(h)
	for _, gone := range []string{"Connection", "X-Custom", "Proxy-Connection"} {
		if h.Get(gone) != "" {
			t.Errorf("%s should have been stripped", gone)
		}
	}
	if h.Get("Authorization") != "keep-me" {
		t.Error("Authorization is end-to-end and must survive forwarding")
	}
}
