package gaterun_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
