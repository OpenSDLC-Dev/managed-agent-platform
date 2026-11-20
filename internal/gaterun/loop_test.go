package gaterun_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gate"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gateconfig"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/gaterun"
)

type fakeFetcher struct {
	mu   sync.Mutex
	call int
	fn   func(call int) (*gateconfig.Config, error)
}

func (f *fakeFetcher) Fetch(context.Context) (*gateconfig.Config, error) {
	f.mu.Lock()
	n := f.call
	f.call++
	f.mu.Unlock()
	return f.fn(n)
}

func unrestrictedGate() *gate.Gate {
	return gate.New(gate.Config{Networking: domain.Networking{Type: domain.NetUnrestricted}})
}

func TestRunFetchLoopUnauthorizedIsTerminal(t *testing.T) {
	// The initial gate is permissive (502 on an admitted host). An ErrUnauthorized
	// must flip it deny-all (403) AND end the loop: a revoked token stops serving.
	h := gaterun.NewSwappableHandler(unrestrictedGate())
	srv := httptest.NewServer(h)
	defer srv.Close()
	if got := proxyStatus(t, srv.URL, unreachableHost); got != http.StatusBadGateway {
		t.Fatalf("precondition: initial status = %d, want 502", got)
	}

	f := &fakeFetcher{fn: func(int) (*gateconfig.Config, error) { return nil, gateconfig.ErrUnauthorized }}
	err := gaterun.RunFetchLoop(context.Background(), f, h, time.Hour)
	if !errors.Is(err, gateconfig.ErrUnauthorized) {
		t.Fatalf("RunFetchLoop = %v, want ErrUnauthorized", err)
	}
	if got := proxyStatus(t, srv.URL, unreachableHost); got != http.StatusForbidden {
		t.Errorf("after ErrUnauthorized: status = %d, want 403 (deny-all)", got)
	}
}

func TestRunFetchLoopSwapsThenKeepsLastGood(t *testing.T) {
	// Initial deny-all (403). Fetch 0 returns an unrestricted config → swap to
	// permissive (502). Every later fetch errors transiently; the gate must stay
	// permissive (last-known-good), never revert to deny-all on a control-plane blip.
	h := gaterun.NewSwappableHandler(gate.New(gate.Config{}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	secondFetch := make(chan struct{})
	var once sync.Once
	f := &fakeFetcher{fn: func(call int) (*gateconfig.Config, error) {
		if call == 0 {
			return &gateconfig.Config{Networking: domain.Networking{Type: domain.NetUnrestricted}}, nil
		}
		// Reached only after fetch 0 returned and the loop swapped (happens-before),
		// so the permissive gate is guaranteed visible once this fires.
		once.Do(func() { close(secondFetch) })
		return nil, errors.New("controlplane blip")
	}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gaterun.RunFetchLoop(ctx, f, h, time.Millisecond) }()

	select {
	case <-secondFetch:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("loop never fetched a second time")
	}
	if got := proxyStatus(t, srv.URL, unreachableHost); got != http.StatusBadGateway {
		t.Errorf("after swap + transient error: status = %d, want 502 (last-known-good kept)", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("loop returned %v on cancel, want nil", err)
	}
}

func TestRunFetchLoopContextCancelReturnsNil(t *testing.T) {
	h := gaterun.NewSwappableHandler(gate.New(gate.Config{}))
	f := &fakeFetcher{fn: func(int) (*gateconfig.Config, error) { return nil, errors.New("blip") }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gaterun.RunFetchLoop(ctx, f, h, time.Millisecond) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunFetchLoop = %v on cancel, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not return after cancel")
	}
}

func TestRunFetchLoopSharesLimiterAcrossSwaps(t *testing.T) {
	// Exercise real HTTP traffic across a RunFetchLoop swap: a request started
	// before the swap holds the single concurrency slot, and a second request
	// hitting the newly swapped gate must observe 503 until the first finishes.
	reqInFlight := make(chan struct{}, 1)
	releaseOrigin := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOrigin) }) }
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			reqInFlight <- struct{}{}
			<-releaseOrigin
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	defer release()
	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	originHost := originURL.Hostname()

	initial := gate.New(gate.Config{
		Networking:            domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{originHost}},
		MaxConcurrentRequests: 1,
	})
	h := gaterun.NewSwappableHandler(initial)
	gsrv := httptest.NewServer(h)
	defer gsrv.Close()
	proxyURL, err := url.Parse(gsrv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	// Start request 1 on the initial gate and hold it at the origin.
	reqDone := make(chan error, 1)
	go func() {
		resp, err := client.Get(origin.URL + "/hold")
		if err != nil {
			reqDone <- err
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			reqDone <- errors.New("unexpected status from initial held request")
			return
		}
		reqDone <- nil
	}()
	select {
	case <-reqInFlight:
	case err := <-reqDone:
		t.Fatalf("initial request exited before reaching origin: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial request to reach origin")
	}

	// Now swap in a newly fetched gate while request 1 still holds the 1 slot.
	// Fetch 1 runs only after fetch 0's handler.Swap has completed.
	swapped := make(chan struct{})
	var once sync.Once
	f := &fakeFetcher{fn: func(call int) (*gateconfig.Config, error) {
		if call >= 1 {
			once.Do(func() { close(swapped) })
		}
		return &gateconfig.Config{
			Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{originHost}},
		}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- gaterun.RunFetchLoop(ctx, f, h, time.Millisecond) }()
	select {
	case <-swapped:
	case <-time.After(2 * time.Second):
		t.Fatal("loop never swapped")
	}

	// Request 2 hits the swapped gate while request 1 is still active on the old
	// gate: because they share one Limiter, it must be rejected with 503.
	overResp, err := client.Get(origin.URL + "/fast")
	if err != nil {
		t.Fatal(err)
	}
	overResp.Body.Close()
	if overResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("request after swap while 1 slot held: status = %d, want 503", overResp.StatusCode)
	}

	// Releasing request 1 frees the shared slot so request 3 succeeds on the swapped gate.
	release()
	if err := <-reqDone; err != nil {
		t.Fatalf("initial request failed: %v", err)
	}
	okResp, err := client.Get(origin.URL + "/fast")
	if err != nil {
		t.Fatal(err)
	}
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("request after release on swapped gate: status = %d, want 200", okResp.StatusCode)
	}

	cancel()
	<-done
}

func TestRunFetchLoopRevocationClosesActiveTunnel(t *testing.T) {
	// Echo TCP listener representing an upstream HTTPS endpoint.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()

	h := gaterun.NewSwappableHandler(gate.New(gate.Config{
		Networking: domain.Networking{Type: domain.NetLimited, AllowedHosts: []string{"127.0.0.1"}},
	}))
	gsrv := httptest.NewServer(h)
	defer gsrv.Close()

	// Establish a live CONNECT tunnel before the session token is revoked.
	conn, err := net.DialTimeout("tcp", gsrv.Listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr().String(), ln.Addr().String())
	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT setup = %v / %v, want 200", resp, err)
	}
	if _, err := io.WriteString(conn, "hi"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "hi" {
		t.Fatalf("tunnel echo = %q / %v, want hi", buf, err)
	}

	// Revoke via ErrUnauthorized in RunFetchLoop: the live tunnel must close immediately.
	f := &fakeFetcher{fn: func(int) (*gateconfig.Config, error) { return nil, gateconfig.ErrUnauthorized }}
	if err := gaterun.RunFetchLoop(context.Background(), f, h, time.Hour); !errors.Is(err, gateconfig.ErrUnauthorized) {
		t.Fatalf("RunFetchLoop = %v, want ErrUnauthorized", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	one := make([]byte, 1)
	if _, err := br.Read(one); err == nil {
		t.Fatal("active CONNECT tunnel stayed open after ErrUnauthorized revocation")
	} else {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("active CONNECT tunnel stayed open until read deadline expired: %v", err)
		}
	}
}
