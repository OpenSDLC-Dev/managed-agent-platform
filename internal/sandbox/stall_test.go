package sandbox_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
			// Hold response headers until the entire streaming upload finishes,
			// so the upload alone must outlive budget via request-body progress.
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

		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		go func() {
			defer pw.Close()
			for i := 0; i < 6; i++ {
				if _, err := pw.Write([]byte("x")); err != nil {
					return
				}
				<-time.After(budget / 3)
			}
		}()

		client := &http.Client{Transport: sandbox.GuardRoundTripper(srv.Client().Transport, budget)}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, pr)
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

	t.Run("request body cancel remains armed after response headers arrive", func(t *testing.T) {
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })

		uploadDone := make(chan error, 1)
		rt := sandbox.GuardRoundTripper(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			// Simulate Go net/http receiving response headers while a background
			// writeLoop is still blocked reading req.Body.
			go func() {
				_, err := io.ReadAll(req.Body)
				uploadDone <- err
			}()
			respPR, respPW := io.Pipe()
			t.Cleanup(func() { _ = respPW.Close() })
			return &http.Response{StatusCode: http.StatusOK, Body: respPR}, nil
		}), 80*time.Millisecond)

		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.invalid", pr)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer resp.Body.Close()

		// Once the silence budget expires, the guard cancels gctx, which must
		// still close req.Body and unblock the background upload reader even
		// though RoundTrip already returned response headers.
		select {
		case err := <-uploadDone:
			if err == nil {
				t.Errorf("uploadDone = nil, want closed-pipe error after stall trip")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("background req.Body reader stayed blocked after response headers arrived and guard tripped")
		}
	})

	t.Run("nil response body stops guard without panic", func(t *testing.T) {
		rt := sandbox.GuardRoundTripper(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNoContent, Body: nil}, nil
		}), 50*time.Millisecond)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.invalid", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		if resp.Body != nil {
			t.Errorf("resp.Body = %v, want nil", resp.Body)
		}
	})

	t.Run("request body close is idempotent under concurrent cancel", func(t *testing.T) {
		var closes atomic.Int32
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		body := &countingCloser{ReadCloser: pr, closes: &closes}

		rt := sandbox.GuardRoundTripper(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			_, err := io.ReadAll(req.Body)
			_ = req.Body.Close()
			return nil, err
		}), 40*time.Millisecond)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.invalid", body)
		if err != nil {
			t.Fatal(err)
		}
		_, err = rt.RoundTrip(req)
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("err = %v, want ErrStalled", err)
		}
		if got := closes.Load(); got != 1 {
			t.Errorf("underlying body Close count = %d, want 1", got)
		}
	})
}

func TestStallGuardStopAndParentCancelDoNotFalselyTrip(t *testing.T) {
	// An already-cancelled parent context racing a tiny timer must never flip
	// the guard to ErrStalled.
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, guard := sandbox.NewStallGuard(ctx, time.Nanosecond)
		// Wait past the timer firing window without sleeping.
		<-time.After(time.Millisecond)
		guard.Stop()
		if err := guard.Cause(context.Canceled); errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("iter %d: already-cancelled parent misclassified as ErrStalled: %v", i, err)
		}
	}

	// Calling Stop() before the budget expires must prevent watch() from ever
	// setting tripped later, even if we pause past the original budget before
	// calling Cause().
	for i := 0; i < 50; i++ {
		_, guard := sandbox.NewStallGuard(context.Background(), 2*time.Millisecond)
		guard.Stop()
		<-time.After(5 * time.Millisecond)
		sentinel := errors.New("real transport error")
		if err := guard.Cause(sentinel); !errors.Is(err, sentinel) || errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("iter %d: Cause after Stop = %v, want %v", i, err, sentinel)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type countingCloser struct {
	io.ReadCloser
	closes *atomic.Int32
}

func (c *countingCloser) Close() error {
	c.closes.Add(1)
	return c.ReadCloser.Close()
}
