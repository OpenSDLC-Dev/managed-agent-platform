package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

func TestParseStallTimeout(t *testing.T) {
	if got, err := sandbox.ParseStallTimeout("SANDBOX_STALL_TIMEOUT", ""); err != nil || got != 0 {
		t.Fatalf("empty = (%s, %v), want (0, nil)", got, err)
	}
	if got, err := sandbox.ParseStallTimeout("SANDBOX_STALL_TIMEOUT", "2m30s"); err != nil || got != 150*time.Second {
		t.Fatalf("2m30s = (%s, %v), want (2m30s, nil)", got, err)
	}
	for _, bad := range []string{"nope", "0", "0s", "-5s"} {
		if _, err := sandbox.ParseStallTimeout("SANDBOX_STALL_TIMEOUT", bad); err == nil {
			t.Errorf("ParseStallTimeout(%q) = nil error, want refusal", bad)
		}
	}
}

func TestStallGuardTripsAfterSilence(t *testing.T) {
	const budget = 100 * time.Millisecond
	start := time.Now()
	gctx, guard := sandbox.NewStallGuard(context.Background(), budget)
	defer guard.Stop()

	select {
	case <-gctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("guard did not trip after silence")
	}
	if elapsed := time.Since(start); elapsed < budget {
		t.Errorf("guard tripped in %s, before its %s budget", elapsed, budget)
	}
	err := guard.Cause(context.Canceled)
	if !errors.Is(err, sandbox.ErrStalled) {
		t.Fatalf("Cause(context.Canceled) = %v, want ErrStalled", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Cause wrapped context.Canceled (%v); callers must distinguish a stall from cancellation", err)
	}
}

func TestStallGuardProgressKeepsContextAlive(t *testing.T) {
	const budget = 150 * time.Millisecond
	gctx, guard := sandbox.NewStallGuard(context.Background(), budget)
	defer guard.Stop()

	ticker := time.NewTicker(budget / 5)
	defer ticker.Stop()
	deadline := time.After(2 * budget)
	for {
		select {
		case <-gctx.Done():
			t.Fatal("guard tripped while progress was still being reported")
		case <-ticker.C:
			guard.Progress()
		case <-deadline:
			if err := guard.Cause(nil); err != nil {
				t.Fatalf("Cause(nil) while alive = %v, want nil", err)
			}
			return
		}
	}
}

func TestStallGuardDisarmStopsWatchingWithoutCancelling(t *testing.T) {
	const budget = 80 * time.Millisecond
	gctx, guard := sandbox.NewStallGuard(context.Background(), budget)
	defer guard.Stop()

	if !guard.Disarm() {
		t.Fatal("Disarm before tripping returned false")
	}
	select {
	case <-gctx.Done():
		t.Fatal("disarmed guard cancelled context")
	case <-time.After(2 * budget):
	}
	if err := guard.Cause(nil); err != nil {
		t.Errorf("Cause(nil) after Disarm = %v, want nil", err)
	}
}

func TestStallGuardDisarmAfterTripReportsFalse(t *testing.T) {
	const budget = 50 * time.Millisecond
	gctx, guard := sandbox.NewStallGuard(context.Background(), budget)
	defer guard.Stop()

	select {
	case <-gctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("guard did not trip")
	}
	if guard.Disarm() {
		t.Error("Disarm after tripping returned true, want false")
	}
}

func TestStallGuardCallerCancellationIsNotAStall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	gctx, guard := sandbox.NewStallGuard(ctx, 10*time.Second)
	defer guard.Stop()

	cancel()
	select {
	case <-gctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("child context did not observe parent cancellation")
	}
	err := guard.Cause(context.Canceled)
	if errors.Is(err, sandbox.ErrStalled) {
		t.Errorf("caller cancellation misclassified as ErrStalled: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Cause(context.Canceled) = %v, want context.Canceled", err)
	}
}

func TestStallGuardDefaultBudget(t *testing.T) {
	_, guard := sandbox.NewStallGuard(context.Background(), 0)
	defer guard.Stop()
	if got := guard.Budget(); got != sandbox.DefaultStallTimeout {
		t.Errorf("Budget(0) = %s, want DefaultStallTimeout (%s)", got, sandbox.DefaultStallTimeout)
	}
	if sandbox.DefaultStallTimeout < time.Minute {
		t.Errorf("DefaultStallTimeout = %s: too tight for silent platform operations", sandbox.DefaultStallTimeout)
	}
}

func TestGuardReadCloserReplacesEOFOnTrip(t *testing.T) {
	gctx, guard := sandbox.NewStallGuard(context.Background(), 50*time.Millisecond)
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	rc := sandbox.GuardReadCloser(pr, guard)
	defer rc.Close()

	go func() {
		<-gctx.Done()
		// Simulate an HTTP/1.0 close-delimited stream returning plain EOF on socket close.
		_ = pw.Close()
	}()

	var buf [16]byte
	_, err := rc.Read(buf[:])
	if !errors.Is(err, sandbox.ErrStalled) {
		t.Errorf("Read after trip = %v, want ErrStalled", err)
	}
}

func TestGuardRoundTripperBoundsWedgedAndMovingHTTP(t *testing.T) {
	const budget = 150 * time.Millisecond

	t.Run("wedged headers", func(t *testing.T) {
		unblock := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-unblock
		}))
		t.Cleanup(func() {
			close(unblock)
			srv.Close()
		})
		client := &http.Client{Transport: sandbox.GuardRoundTripper(srv.Client().Transport, budget)}
		_, err := client.Get(srv.URL)
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged GET err = %v, want ErrStalled", err)
		}
	})

	t.Run("wedged body", func(t *testing.T) {
		unblock := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "hello")
			w.(http.Flusher).Flush()
			<-unblock
		}))
		t.Cleanup(func() {
			close(unblock)
			srv.Close()
		})
		client := &http.Client{Transport: sandbox.GuardRoundTripper(srv.Client().Transport, budget)}
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged body ReadAll = %v, want ErrStalled", err)
		}
	})

	t.Run("streaming upload and download outlive budget", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			for i := 0; i < 6; i++ {
				_, _ = w.Write(body[:1])
				flusher.Flush()
				select {
				case <-time.After(budget / 3):
				case <-r.Context().Done():
					return
				}
			}
		}))
		t.Cleanup(srv.Close)

		client := &http.Client{Transport: sandbox.GuardRoundTripper(srv.Client().Transport, budget)}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, bytes.NewReader([]byte("x")))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		if string(got) != strings.Repeat("x", 6) {
			t.Errorf("got %q, want 6 x's", got)
		}
	})
}
