package gate_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/egress"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gate"
)

func assertConnClosed(t *testing.T, c net.Conn, br *bufio.Reader, label string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	one := make([]byte, 1)
	n, err := br.Read(one)
	if err == nil {
		t.Fatalf("%s stayed open and delivered %d byte(s)", label, n)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("%s stayed open until read deadline expired (not closed by server): %v", label, err)
	}
}

func TestGatePlainHTTPWithdrawnBeforeOutboundCannotDial(t *testing.T) {
	var hits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	host := hostOf(t, origin.URL)

	g := gate.New(gate.Config{
		Networking:            domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		MaxBodyBytes:          1024,
		MaxTotalBodyBytes:     1500,
		MaxConcurrentRequests: 4,
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// Begin a chunked POST under Policy v1 (origin allowed) and pause after
	// sending the first chunk so the handler is mid-request when policy swaps.
	conn, err := net.DialTimeout("tcp", gsrv.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST %s/work HTTP/1.1\r\nHost: %s\r\nTransfer-Encoding: chunked\r\n\r\n400\r\n%s\r\n",
		origin.URL, host, strings.Repeat("A", 1024))

	// Wait until the gate has buffered the 1024-byte chunk (observed when a
	// second 1024-byte POST hits the 1500-byte aggregate budget with 503).
	client := proxyClient(t, gsrv.URL, nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		probe, err := client.Post(origin.URL+"/probe", "text/plain", strings.NewReader(strings.Repeat("B", 1024)))
		if err != nil {
			t.Fatal(err)
		}
		probe.Body.Close()
		if probe.StatusCode == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gate never buffered first chunk; last probe status = %d", probe.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
	hitsBefore := hits.Load()

	// Withdraw the host while the request is mid-body.
	g.Update(gate.Config{Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"other.invalid"}}})

	// Complete the body: the handler must apply the current policy at outbound
	// admission, refuse with 403, and never dial origin.
	_, _ = io.WriteString(conn, "0\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("withdrawn mid-body status = %d, want 403", resp.StatusCode)
	}
	if got := hits.Load() - hitsBefore; got != 0 {
		t.Fatalf("origin received %d requests after policy withdrawal, want 0", got)
	}
}

func TestGatePlainHTTPCredentialWithdrawnBeforeOutboundNotSubstituted(t *testing.T) {
	var gotHeader, gotBody string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Token")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	host := hostOf(t, origin.URL)

	const placeholder = "vltph_withdrawn_cred_00000000000000"
	const secret = "super-secret-value-must-not-leak"

	g := gate.New(gate.Config{
		Networking:            domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		MaxBodyBytes:          2048,
		MaxTotalBodyBytes:     1500,
		MaxConcurrentRequests: 4,
		Credentials: []egress.Credential{{
			Placeholder: placeholder,
			Secret:      secret,
			Hosts:       egress.NewHostSet([]string{host}),
			Header:      true,
			Body:        true,
		}},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	// Begin a chunked POST carrying the placeholder in both header and body,
	// pausing mid-body after buffering 1024 bytes.
	payload := placeholder + strings.Repeat("x", 1024-len(placeholder))
	conn, err := net.DialTimeout("tcp", gsrv.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST %s/cred HTTP/1.1\r\nHost: %s\r\nX-Token: %s\r\nTransfer-Encoding: chunked\r\n\r\n400\r\n%s\r\n",
		origin.URL, host, placeholder, payload)

	client := proxyClient(t, gsrv.URL, nil)
	deadline := time.Now().Add(2 * time.Second)
	for {
		probe, err := client.Post(origin.URL+"/probe", "text/plain", strings.NewReader(strings.Repeat("B", 1024)))
		if err != nil {
			t.Fatal(err)
		}
		probe.Body.Close()
		if probe.StatusCode == http.StatusServiceUnavailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gate never buffered first chunk; last probe status = %d", probe.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Keep the host allowed, but withdraw the credential before the body finishes.
	g.Update(gate.Config{
		Networking:  domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
		Credentials: nil,
	})

	_, _ = io.WriteString(conn, "0\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotHeader != placeholder || strings.Contains(gotHeader, secret) {
		t.Fatalf("header = %q, want literal placeholder (secret withdrawn)", gotHeader)
	}
	if !strings.HasPrefix(gotBody, placeholder) || strings.Contains(gotBody, secret) {
		t.Fatalf("body leaked withdrawn secret or missed literal placeholder: %q", gotBody[:min(64, len(gotBody))])
	}
}

func TestGateActivePlainHTTPStreamCancelledOnWithdrawalAndUnaffectedSurvives(t *testing.T) {
	releaseA := make(chan struct{})
	var relAOnce sync.Once
	endA := func() { relAOnce.Do(func() { close(releaseA) }) }

	originA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "start\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-releaseA:
			_, _ = io.WriteString(w, "end\n")
		case <-r.Context().Done():
		}
	}))
	defer originA.Close()
	defer endA()

	hostA := hostOf(t, originA.URL)
	const placeholder = "vltph_stream_cred_00000000000000000"
	const secret = "stream-secret"

	g := gate.New(gate.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{hostA}},
		Credentials: []egress.Credential{
			{Placeholder: placeholder, Secret: secret, Hosts: egress.NewHostSet([]string{hostA}), Header: true},
			{Placeholder: "vltph_unrelated_000000000000000000", Secret: "other", Hosts: egress.NewHostSet([]string{hostA}), Header: true},
		},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()
	client := proxyClient(t, gsrv.URL, nil)

	startStream := func(withCred bool) (*http.Response, chan error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		t.Cleanup(cancel)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, originA.URL+"/stream", nil)
		if err != nil {
			t.Fatal(err)
		}
		if withCred {
			req.Header.Set("Authorization", "Bearer "+placeholder)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream status = %d, want 200", resp.StatusCode)
		}
		// Read first chunk to ensure the stream is live.
		buf := make([]byte, 6)
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatalf("read initial stream bytes: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			done <- err
		}()
		return resp, done
	}

	// Start two streams: one carrying the credential, one without it.
	_, credDone := startStream(true)
	_, plainDone := startStream(false)

	// Withdrawing only the UNRELATED credential leaves both streams alive.
	g.Update(gate.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{hostA}},
		Credentials: []egress.Credential{
			{Placeholder: placeholder, Secret: secret, Hosts: egress.NewHostSet([]string{hostA}), Header: true},
		},
	})
	select {
	case err := <-credDone:
		t.Fatalf("unrelated credential update cancelled cred stream: %v", err)
	case err := <-plainDone:
		t.Fatalf("unrelated credential update cancelled plain stream: %v", err)
	case <-time.After(80 * time.Millisecond):
	}

	// Now withdraw the credential used by credDone while keeping hostA allowed:
	// credDone must abort with a non-nil stream error (not a clean EOF) while
	// plainDone stays alive.
	g.Update(gate.Config{
		Networking:  domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{hostA}},
		Credentials: nil,
	})
	select {
	case err := <-credDone:
		if err == nil {
			t.Fatal("withdrawn credential stream ended with clean EOF instead of abort error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active stream using withdrawn credential was not cancelled")
	}
	select {
	case err := <-plainDone:
		t.Fatalf("plain stream without withdrawn credential was unexpectedly cancelled: %v", err)
	case <-time.After(80 * time.Millisecond):
	}

	// Tightening from operator-vouched hostA (unfloored) to unrestricted
	// (floored over loopback 127.0.0.1) must abort plainDone with a stream error too.
	g.Update(gate.Config{Networking: domain.Networking{Type: domain.NetUnrestricted}})
	select {
	case err := <-plainDone:
		if err == nil {
			t.Fatal("floor-withdrawn stream ended with clean EOF instead of abort error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active stream over loopback was not cancelled on floor tightening")
	}
	endA()
}

func TestGateConnectTunnelWithdrawnImmediatelyAndUnaffectedSurvives(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port)
	var mu sync.Mutex
	var serverConns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			serverConns = append(serverConns, c)
			mu.Unlock()
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range serverConns {
			_ = c.Close()
		}
		mu.Unlock()
	}()

	g := gate.New(gate.Config{
		Networking:           domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"127.0.0.1", "localhost"}},
		MaxConcurrentTunnels: 2,
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()

	openTunnel := func(host string) (net.Conn, *bufio.Reader) {
		t.Helper()
		target := net.JoinHostPort(host, port)
		conn, err := net.DialTimeout("tcp", gsrv.Listener.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		br := bufio.NewReader(conn)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			t.Fatalf("CONNECT %s status = %d, want 200", target, resp.StatusCode)
		}
		return conn, br
	}

	echo := func(c net.Conn, br *bufio.Reader, msg string) error {
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.WriteString(c, msg); err != nil {
			return err
		}
		buf := make([]byte, len(msg))
		_, err := io.ReadFull(br, buf)
		if err != nil {
			return err
		}
		if string(buf) != msg {
			return fmt.Errorf("echo %q, want %q", buf, msg)
		}
		return nil
	}

	c1, br1 := openTunnel("127.0.0.1")
	defer c1.Close()
	c2, br2 := openTunnel("localhost")
	defer c2.Close()

	if err := echo(c1, br1, "ping1"); err != nil {
		t.Fatalf("c1 before update: %v", err)
	}
	if err := echo(c2, br2, "ping2"); err != nil {
		t.Fatalf("c2 before update: %v", err)
	}

	// Withdraw 127.0.0.1 while keeping localhost permitted: c1 must close
	// immediately (non-timeout EOF/close) and c2 must continue echoing uninterrupted.
	g.Update(gate.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"localhost"}},
	})

	assertConnClosed(t, c1, br1, "withdrawn CONNECT tunnel c1")
	if err := echo(c2, br2, "still-alive"); err != nil {
		t.Fatalf("unaffected CONNECT tunnel c2 failed after unrelated host withdrawal: %v", err)
	}

	// Tightening policy from operator-vouched localhost (unfloored) to
	// unrestricted (floored by default dialguard.IPAllowed, which rejects
	// loopback) must also close c2 immediately.
	g.Update(gate.Config{
		Networking: domain.Networking{Type: domain.NetUnrestricted},
	})
	assertConnClosed(t, c2, br2, "floor-tightened CONNECT tunnel c2")
}

func TestGateIdleConnectionsClosedOnPolicyWithdrawal(t *testing.T) {
	var hits atomic.Int32
	connClosed := make(chan struct{}, 4)
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case connClosed <- struct{}{}:
			default:
			}
		}
	}
	origin.Start()
	defer origin.Close()
	host := hostOf(t, origin.URL)

	g := gate.New(gate.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{host}},
	})
	gsrv := httptest.NewServer(g)
	defer gsrv.Close()
	client := proxyClient(t, gsrv.URL, nil)

	// Warm a pooled keep-alive connection to loopback origin under operator-vouched
	// (unfloored) allowed_hosts.
	resp, err := client.Get(origin.URL + "/warm")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("warm request = %d (hits=%d), want 200 (1)", resp.StatusCode, hits.Load())
	}

	// Switch to unrestricted (which still passes host admission into RoundTrip
	// but enforces the loopback address floor). The warm idle socket MUST close
	// and never be reused by RoundTrip — forcing a fresh dial that hits the
	// floor and returns 403 with zero additional origin hits.
	g.Update(gate.Config{Networking: domain.Networking{Type: domain.NetUnrestricted}})
	select {
	case <-connClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("warm idle upstream connection was not closed on Gate.Update")
	}

	resp2, err := client.Get(origin.URL + "/after")
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden || !strings.Contains(string(body2), "private/reserved") {
		t.Fatalf("post-update status = %d (%q), want 403 floor refusal (not reused pooled socket)", resp2.StatusCode, body2)
	}
	if hits.Load() != 1 {
		t.Fatalf("origin hits = %d, want 1 (reused warm socket)", hits.Load())
	}
}
