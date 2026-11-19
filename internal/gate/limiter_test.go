package gate_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gate"
)

func TestGateConcurrentRequestAndTunnelLimits(t *testing.T) {
	reqArrived := make(chan struct{}, 2)
	releaseOrigin := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			reqArrived <- struct{}{}
			<-releaseOrigin
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()
	host := hostOf(t, origin.URL)

	// Echo TCP listener for CONNECT tunnels.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var tunnelConns sync.Mutex
	var openServerConns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			tunnelConns.Lock()
			openServerConns = append(openServerConns, c)
			tunnelConns.Unlock()
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	defer func() {
		tunnelConns.Lock()
		for _, c := range openServerConns {
			_ = c.Close()
		}
		tunnelConns.Unlock()
	}()

	g := gate.New(gate.Config{
		Networking:            domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host, "127.0.0.1"}},
		MaxConcurrentRequests: 2,
		MaxConcurrentTunnels:  2,
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()
	client := proxyClient(t, gsrv.URL, nil)

	// Fill both plain-HTTP request slots.
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(origin.URL + "/hold")
			if err != nil {
				t.Errorf("held request: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	for range 2 {
		<-reqArrived
	}

	// A 3rd plain-HTTP request is rejected with 503 while both slots are held.
	overResp, err := client.Get(origin.URL + "/fast")
	if err != nil {
		t.Fatal(err)
	}
	overResp.Body.Close()
	if overResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("3rd concurrent plain-HTTP status = %d, want 503", overResp.StatusCode)
	}

	// CONNECT tunnels have an independent slot budget, so opening 2 tunnels
	// succeeds even while plain-HTTP slots are full.
	openTunnel := func() (net.Conn, *bufio.Reader, int) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", gsrv.Listener.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr().String(), ln.Addr().String())
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		return conn, br, resp.StatusCode
	}
	t1, _, s1 := openTunnel()
	t2, _, s2 := openTunnel()
	if s1 != http.StatusOK || s2 != http.StatusOK {
		t.Fatalf("tunnel 1/2 statuses = %d/%d, want 200/200", s1, s2)
	}

	// A 3rd CONNECT tunnel is rejected with 503.
	t3, _, s3 := openTunnel()
	t3.Close()
	if s3 != http.StatusServiceUnavailable {
		t.Fatalf("3rd concurrent tunnel status = %d, want 503", s3)
	}

	// Releasing the plain-HTTP requests lets ordinary plain-HTTP traffic
	// succeed even while both CONNECT tunnels remain open.
	close(releaseOrigin)
	wg.Wait()
	okResp, err := client.Get(origin.URL + "/fast")
	if err != nil {
		t.Fatal(err)
	}
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("plain-HTTP while tunnels open: status = %d, want 200", okResp.StatusCode)
	}

	// Closing one tunnel frees a tunnel slot.
	t1.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, _, st := openTunnel()
		c.Close()
		if st == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnel slot not released after close; last status = %d", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t2.Close()
}

func TestGateAggregateBodyBufferBudget(t *testing.T) {
	originHold := make(chan struct{}, 1)
	releaseOrigin := make(chan struct{})
	streamStart := make(chan struct{}, 1)
	releaseStream := make(chan struct{})

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/hold":
			originHold <- struct{}{}
			<-releaseOrigin
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case "/stream":
			// Flush response headers right away so RoundTrip returns on the gate
			// while the response body is still open.
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "chunk1")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			streamStart <- struct{}{}
			<-releaseStream
			_, _ = io.WriteString(w, "chunk2")
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}
	}))
	defer origin.Close()
	host := hostOf(t, origin.URL)

	g := gate.New(gate.Config{
		Networking:            domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		MaxBodyBytes:          1024,
		MaxTotalBodyBytes:     1500,
		MaxConcurrentRequests: 8,
		Credentials: []egress.Credential{{
			Placeholder: "vltph_short",
			Secret:      strings.Repeat("S", 900),
			Hosts:       egress.NewHostSet([]string{host}),
			Body:        true,
		}},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()
	client := proxyClient(t, gsrv.URL, nil)

	// Request 1 holds 1024 bytes of the 1500-byte aggregate budget while waiting
	// for origin response headers.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := client.Post(origin.URL+"/hold", "text/plain", strings.NewReader(strings.Repeat("A", 1024)))
		if err != nil {
			t.Errorf("held post: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	<-originHold

	// Request 2 (known Content-Length 1024) exceeds remaining 476-byte budget → 503.
	overDeclared, err := client.Post(origin.URL+"/fast", "text/plain", strings.NewReader(strings.Repeat("B", 1024)))
	if err != nil {
		t.Fatal(err)
	}
	overDeclared.Body.Close()
	if overDeclared.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-budget declared body status = %d, want 503", overDeclared.StatusCode)
	}

	// Request 3 (chunked, unknown Content-Length -1) exceeds remaining budget while reading → 503.
	chunkedReq, err := http.NewRequest(http.MethodPost, origin.URL+"/fast", io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("C"), 1024))))
	if err != nil {
		t.Fatal(err)
	}
	chunkedReq.ContentLength = -1
	overChunked, err := client.Do(chunkedReq)
	if err != nil {
		t.Fatal(err)
	}
	overChunked.Body.Close()
	if overChunked.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("over-budget chunked body status = %d, want 503", overChunked.StatusCode)
	}

	// Request 4: secret substitution expands a small placeholder beyond the
	// remaining aggregate budget (900 > 476) → 503 and releases its bytes.
	overSubst, err := client.Post(origin.URL+"/fast", "text/plain", strings.NewReader("vltph_short"))
	if err != nil {
		t.Fatal(err)
	}
	overSubst.Body.Close()
	if overSubst.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("substitution expansion over aggregate budget status = %d, want 503", overSubst.StatusCode)
	}

	// Request 5: a body larger than MaxBodyBytes (1025 > 1024) still gets 413,
	// not 503, for both declared and chunked Content-Length.
	tooBig, err := client.Post(origin.URL+"/fast", "text/plain", strings.NewReader(strings.Repeat("D", 1025)))
	if err != nil {
		t.Fatal(err)
	}
	tooBig.Body.Close()
	if tooBig.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over MaxBodyBytes status = %d, want 413", tooBig.StatusCode)
	}

	// Ordinary GET and small POST still succeed while Request 1 holds 1024 bytes.
	getResp, err := client.Get(origin.URL + "/fast")
	if err != nil {
		t.Fatal(err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("ordinary GET under load status = %d, want 200", getResp.StatusCode)
	}
	smallResp, err := client.Post(origin.URL+"/fast", "text/plain", strings.NewReader(strings.Repeat("E", 200)))
	if err != nil {
		t.Fatal(err)
	}
	smallBody, _ := io.ReadAll(smallResp.Body)
	smallResp.Body.Close()
	if smallResp.StatusCode != http.StatusOK || len(smallBody) != 200 {
		t.Fatalf("small POST under load = %d (%d bytes), want 200 (200 bytes)", smallResp.StatusCode, len(smallBody))
	}

	// Release Request 1 and prove the full 1024-byte budget is available again.
	close(releaseOrigin)
	wg.Wait()

	// Also prove a slow-streaming response releases the request-body buffer
	// budget once RoundTrip returns, BEFORE the response stream ends.
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		resp, err := client.Post(origin.URL+"/stream", "text/plain", strings.NewReader(strings.Repeat("F", 1024)))
		if err != nil {
			t.Errorf("stream post: %v", err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	<-streamStart

	// Poll briefly for RoundTrip to finish releasing the body budget on the gate.
	deadline := time.Now().Add(2 * time.Second)
	for {
		r2, err := client.Post(origin.URL+"/fast", "text/plain", strings.NewReader(strings.Repeat("G", 1024)))
		if err != nil {
			t.Fatal(err)
		}
		r2.Body.Close()
		if r2.StatusCode == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("body budget stayed pinned while streaming response; last status = %d", r2.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(releaseStream)
	<-streamDone
}

func TestGateBodyReadTimeoutContainsStalledUpload(t *testing.T) {
	origin := echoOrigin(t)
	defer origin.Close()
	host := hostOf(t, origin.URL)

	g := gate.New(gate.Config{
		Networking:            domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		MaxBodyBytes:          1024,
		MaxTotalBodyBytes:     1024,
		MaxConcurrentRequests: 1,
		BodyReadTimeout:       120 * time.Millisecond,
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// Open raw TCP connection to send headers + partial body and then stall.
	conn, err := net.DialTimeout("tcp", gsrv.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST %s/slow HTTP/1.1\r\nHost: %s\r\nContent-Length: 500\r\n\r\npartial", origin.URL, host)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read timeout response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("stalled body status = %d, want 408", resp.StatusCode)
	}

	// Once the stalled upload times out, its request slot and body buffer are
	// released so an ordinary request succeeds.
	client := proxyClient(t, gsrv.URL, nil)
	okResp, err := client.Post(origin.URL+"/ok", "text/plain", strings.NewReader(strings.Repeat("Z", 1024)))
	if err != nil {
		t.Fatal(err)
	}
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("post after timeout status = %d, want 200", okResp.StatusCode)
	}
}
