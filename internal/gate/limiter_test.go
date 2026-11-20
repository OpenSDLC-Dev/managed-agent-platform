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

func waitSignal(t *testing.T, sig <-chan struct{}, errCh <-chan error, label string) {
	t.Helper()
	select {
	case <-sig:
	case err := <-errCh:
		t.Fatalf("%s worker exited before signal: %v", label, err)
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func TestGateConcurrentRequestAndTunnelLimits(t *testing.T) {
	reqArrived := make(chan struct{}, 2)
	releaseOrigin := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOrigin) }) }
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			reqArrived <- struct{}{}
			<-releaseOrigin
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()
	defer release()
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
	reqErrs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(origin.URL + "/hold")
			if err != nil {
				reqErrs <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				reqErrs <- fmt.Errorf("status %d", resp.StatusCode)
			}
		}()
	}
	for range 2 {
		waitSignal(t, reqArrived, reqErrs, "held plain-HTTP request")
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
	defer t1.Close()
	t2, _, s2 := openTunnel()
	defer t2.Close()
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
	release()
	wg.Wait()
	select {
	case err := <-reqErrs:
		t.Fatalf("held request failed: %v", err)
	default:
	}
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
}

func TestGateAggregateBodyBufferBudget(t *testing.T) {
	streamStart := make(chan struct{}, 1)
	releaseStream := make(chan struct{})
	var releaseStreamOnce sync.Once
	endStream := func() { releaseStreamOnce.Do(func() { close(releaseStream) }) }

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/stream" {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "chunk1")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			streamStart <- struct{}{}
			<-releaseStream
			_, _ = io.WriteString(w, "chunk2")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer origin.Close()
	defer endStream()
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

	// Request 1 sends a 1024-byte chunk on a chunked upload and pauses before
	// the terminating 0-chunk so the gate deterministically holds 1024 bytes of
	// the 1500-byte aggregate budget while we probe concurrent traffic.
	holdConn, err := net.DialTimeout("tcp", gsrv.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer holdConn.Close()
	fmt.Fprintf(holdConn, "POST %s/fast HTTP/1.1\r\nHost: %s\r\nTransfer-Encoding: chunked\r\n\r\n400\r\n%s\r\n",
		origin.URL, host, strings.Repeat("A", 1024))

	// Wait until the gate has read and accounted Request 1's 1024-byte chunk
	// (observed when a 1024-byte declared POST receives 503).
	deadline := time.Now().Add(2 * time.Second)
	for {
		probe, err := client.Post(origin.URL+"/fast", "text/plain", strings.NewReader(strings.Repeat("B", 1024)))
		if err != nil {
			t.Fatal(err)
		}
		probe.Body.Close()
		if probe.StatusCode == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("aggregate budget never observed Request 1's 1024-byte chunk; last status = %d", probe.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
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

	// Request 5: a declared body larger than MaxBodyBytes (1025 > 1024) still gets 413, not 503.
	tooBig, err := client.Post(origin.URL+"/fast", "text/plain", strings.NewReader(strings.Repeat("D", 1025)))
	if err != nil {
		t.Fatal(err)
	}
	tooBig.Body.Close()
	if tooBig.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over MaxBodyBytes (declared) status = %d, want 413", tooBig.StatusCode)
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

	// Finish Request 1's chunked upload and verify it completes cleanly and frees its 1024 bytes.
	_, _ = io.WriteString(holdConn, "0\r\n\r\n")
	_ = holdConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	holdResp, err := http.ReadResponse(bufio.NewReader(holdConn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read Request 1 response: %v", err)
	}
	holdBody, _ := io.ReadAll(holdResp.Body)
	holdResp.Body.Close()
	if holdResp.StatusCode != http.StatusOK || len(holdBody) != 1024 {
		t.Fatalf("Request 1 = %d (%d bytes), want 200 (1024 bytes)", holdResp.StatusCode, len(holdBody))
	}

	// With full budget free, a chunked body exceeding MaxBodyBytes (1025 > 1024)
	// deterministically returns 413 regardless of read boundaries.
	tooBigChunkedReq, err := http.NewRequest(http.MethodPost, origin.URL+"/fast", io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("D"), 1025))))
	if err != nil {
		t.Fatal(err)
	}
	tooBigChunkedReq.ContentLength = -1
	tooBigChunked, err := client.Do(tooBigChunkedReq)
	if err != nil {
		t.Fatal(err)
	}
	tooBigChunked.Body.Close()
	if tooBigChunked.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over MaxBodyBytes (chunked) status = %d, want 413", tooBigChunked.StatusCode)
	}

	// Placeholder expansion exceeding MaxBodyBytes (2 * 900 = 1800 > 1024) is
	// rejected with 413 before allocating an unbounded body.
	bombResp, err := client.Post(origin.URL+"/fast", "text/plain", strings.NewReader("vltph_short vltph_short"))
	if err != nil {
		t.Fatal(err)
	}
	bombResp.Body.Close()
	if bombResp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("placeholder expansion over MaxBodyBytes status = %d, want 413", bombResp.StatusCode)
	}

	// A slow-streaming response releases the upload buffer and budget once
	// http.Transport closes budgetBody after finishing the upload, BEFORE the
	// response stream ends.
	streamErrs := make(chan error, 1)
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		resp, err := client.Post(origin.URL+"/stream", "text/plain", strings.NewReader(strings.Repeat("F", 1024)))
		if err != nil {
			streamErrs <- err
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			streamErrs <- fmt.Errorf("stream status %d", resp.StatusCode)
		}
	}()
	waitSignal(t, streamStart, streamErrs, "streaming origin")

	deadline = time.Now().Add(2 * time.Second)
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
	endStream()
	<-streamDone
	select {
	case err := <-streamErrs:
		t.Fatalf("streaming request failed: %v", err)
	default:
	}
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
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read timeout response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("stalled body status = %d, want 408", resp.StatusCode)
	}
	// Wait for the server to finish rejecting and close the connection so its
	// deferred request slot release has completed before the recovery probe.
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("expected server-side EOF after 408 timeout, got %v", err)
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

func TestGateUploadTimeoutContainsStalledOrigin(t *testing.T) {
	// A raw TCP listener that accepts a connection and never reads the request
	// body simulates a stalled origin that would otherwise pin outbound upload
	// buffers and concurrency slots indefinitely.
	stalledLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stalledLn.Close()
	var mu sync.Mutex
	var heldConns []net.Conn
	go func() {
		for {
			c, err := stalledLn.Accept()
			if err != nil {
				return
			}
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetReadBuffer(4096)
			}
			mu.Lock()
			heldConns = append(heldConns, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range heldConns {
			_ = c.Close()
		}
		mu.Unlock()
	}()

	healthy := echoOrigin(t)
	defer healthy.Close()

	g := gate.New(gate.Config{
		Networking:            domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"127.0.0.1"}},
		MaxBodyBytes:          4 << 20,
		MaxTotalBodyBytes:     3 << 20,
		MaxConcurrentRequests: 1,
		UploadTimeout:         150 * time.Millisecond,
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()
	client := proxyClient(t, gsrv.URL, nil)
	// Bound the client well below the 60s default ResponseHeaderTimeout so a
	// 502 proves UploadTimeout fired rather than ResponseHeaderTimeout.
	client.Timeout = 3 * time.Second

	// Send a 2 MiB body to the non-reading origin so RoundTrip blocks writing
	// until UploadTimeout aborts it.
	resp, err := client.Post("http://"+stalledLn.Addr().String()+"/stall", "application/octet-stream",
		bytes.NewReader(bytes.Repeat([]byte("X"), 2<<20)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("stalled origin upload status = %d, want 502", resp.StatusCode)
	}

	// Both the concurrency slot and the 2 MiB buffer budget must now be free:
	// a second 2 MiB upload cannot fit inside the 3 MiB MaxTotalBodyBytes cap
	// unless the first reservation was released.
	okResp, err := client.Post(healthy.URL+"/ok", "text/plain", bytes.NewReader(bytes.Repeat([]byte("Y"), 2<<20)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, okResp.Body)
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("healthy upload after stalled origin timeout = %d, want 200", okResp.StatusCode)
	}
}

func TestGateStalledBodyOnRejectedRequestsDoesNotPinSlots(t *testing.T) {
	origin := echoOrigin(t)
	defer origin.Close()
	host := hostOf(t, origin.URL)

	// Long BodyReadTimeout (10s) so any unintentional body drain on an early
	// rejection (400/403/413) would hang well past the test's 1s deadline.
	g := gate.New(gate.Config{
		Networking:            domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		MaxBodyBytes:          64,
		MaxTotalBodyBytes:     128,
		MaxConcurrentRequests: 2,
		MaxConcurrentTunnels:  2,
		BodyReadTimeout:       10 * time.Second,
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// Send stalled requests (more than MaxConcurrentRequests=2 and
	// MaxConcurrentTunnels=2) across every early-rejection path without ever
	// sending the declared body bytes. Verify the SERVER closes each
	// connection cleanly under the deadline BEFORE the test closes its socket.
	probes := []struct {
		method string
		raw    string
		want   int
	}{
		{http.MethodPost, "POST http://forbidden.invalid/x HTTP/1.1\r\nHost: forbidden.invalid\r\nContent-Length: 32\r\n\r\n", http.StatusForbidden},
		{http.MethodPost, fmt.Sprintf("POST /not-a-proxy-path HTTP/1.1\r\nHost: %s\r\nContent-Length: 32\r\n\r\n", host), http.StatusBadRequest},
		{http.MethodPost, fmt.Sprintf("POST %s/big HTTP/1.1\r\nHost: %s\r\nContent-Length: 100\r\n\r\n", origin.URL, host), http.StatusRequestEntityTooLarge},
		{http.MethodPost, "POST http://forbidden.invalid/chunked HTTP/1.1\r\nHost: forbidden.invalid\r\nTransfer-Encoding: chunked\r\n\r\n", http.StatusForbidden},
		{http.MethodConnect, "CONNECT forbidden.invalid:443 HTTP/1.1\r\nHost: forbidden.invalid:443\r\nContent-Length: 32\r\n\r\n", http.StatusForbidden},
		{http.MethodConnect, "CONNECT forbidden.invalid:443 HTTP/1.1\r\nHost: forbidden.invalid:443\r\nTransfer-Encoding: chunked\r\n\r\n", http.StatusForbidden},
	}
	for i := range 2 {
		for _, p := range probes {
			conn, err := net.DialTimeout("tcp", gsrv.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatalf("round %d dial: %v", i, err)
			}
			defer conn.Close()
			_, _ = io.WriteString(conn, p.raw)
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			br := bufio.NewReader(conn)
			resp, err := http.ReadResponse(br, &http.Request{Method: p.method})
			if err != nil {
				t.Fatalf("round %d stalled %s rejection (%d) hung: %v", i, p.method, p.want, err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != p.want {
				t.Fatalf("round %d %s status = %d, want %d", i, p.method, resp.StatusCode, p.want)
			}
			// Prove the server closed the connection without hanging on body drain.
			if _, err := br.ReadByte(); err != io.EOF {
				t.Fatalf("round %d %s: expected server-side EOF after rejection, got err=%v", i, p.method, err)
			}
		}
	}

	// All plain-HTTP and tunnel slots must be free so ordinary traffic completes immediately.
	client := proxyClient(t, gsrv.URL, nil)
	okResp, err := client.Get(origin.URL + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("ordinary GET after stalled rejections = %d, want 200", okResp.StatusCode)
	}
	// Verify CONNECT tunnel slots were also released (ordinary allowed CONNECT succeeds).
	originAddr := strings.TrimPrefix(origin.URL, "http://")
	conn, err := net.DialTimeout("tcp", gsrv.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", originAddr, originAddr)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	cResp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("allowed CONNECT after stalled CONNECT rejections: %v", err)
	}
	cResp.Body.Close()
	if cResp.StatusCode != http.StatusOK {
		t.Fatalf("allowed CONNECT after stalled rejections = %d, want 200", cResp.StatusCode)
	}
}
