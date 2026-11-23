package sandboxtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/domain"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/sandbox"
)

// TransportStallBackend stages a sandbox handle wired to a controllable peer
// transport for each wedged-peer scenario, configured with stall as its
// Config.StallTimeout (#395). Both the Docker and Kubernetes backend packages
// run RunTransportStall against their real transports over loopback sockets.
type TransportStallBackend struct {
	// WedgedHandshake returns a sandbox whose peer accepts the exec connection
	// and then sends zero bytes (no HTTP headers on Docker; no HTTP 101
	// Switching Protocols on Kubernetes).
	WedgedHandshake func(t *testing.T, stall time.Duration) sandbox.Sandbox

	// WedgedExecStream returns a sandbox whose peer completes the exec
	// handshake, delivers one stdout chunk, and then goes silent forever
	// (while continuing to answer SPDY keepalive pings on Kubernetes).
	WedgedExecStream func(t *testing.T, stall time.Duration) sandbox.Sandbox

	// WedgedReadFile returns a sandbox whose peer begins answering a file read
	// (tar header + partial bytes on Docker; SPDY 101 + partial stdout on
	// Kubernetes) and then goes silent forever before finishing the file.
	WedgedReadFile func(t *testing.T, stall time.Duration) sandbox.Sandbox

	// WedgedWriteFile returns a sandbox whose peer accepts a WriteFile upload
	// (reading all uploaded bytes) and then goes silent forever before
	// reporting completion (#318's failure shape on Kubernetes).
	WedgedWriteFile func(t *testing.T, stall time.Duration) sandbox.Sandbox
}

// RunTransportStall holds a backend's real transport implementation to the
// silence-bound contract against wedged peers that a live daemon or cluster
// cannot be instructed to simulate.
func RunTransportStall(t *testing.T, b TransportStallBackend) {
	t.Helper()

	const budget = 250 * time.Millisecond

	// A wedged handshake must fail within the stall budget even when the
	// caller supplied a much longer ExecRequest.Timeout: the command has not
	// started yet, and on Kubernetes the HTTP 101 read sits on a raw net.Conn
	// that context cancellation alone cannot interrupt (#395).
	t.Run("WedgedHandshakeFailsWithErrStalled", func(t *testing.T) {
		sb := b.WedgedHandshake(t, budget)
		start := time.Now()
		err := failWithin(t, 5*time.Second, func() error {
			_, err := sb.Exec(context.Background(), sandbox.ExecRequest{
				Command: "echo hi",
				Timeout: 30 * time.Second,
			})
			return err
		})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged handshake err = %v, want sandbox.ErrStalled", err)
		}
		if elapsed := time.Since(start); elapsed < budget {
			t.Errorf("failed in %s, before its %s budget — something other than the stall guard ended it", elapsed, budget)
		}
	})

	// Caller cancellation during a wedged handshake must still unblock
	// promptly (including K8s's raw net.Conn HTTP 101 read) and must NOT be
	// misclassified as ErrStalled. Uses ReadFile (synchronous on both backends,
	// unlike Exec's background goroutine) so a leaked blocked http.ReadResponse
	// cannot hide behind an outer select.
	t.Run("CallerCancellationDuringWedgedHandshakeIsNotErrStalled", func(t *testing.T) {
		sb := b.WedgedHandshake(t, 10*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := failWithin(t, 5*time.Second, func() error {
			_, err := sb.ReadFile(ctx, "/workspace/a.txt")
			return err
		})
		if err == nil {
			t.Fatal("expected cancellation error, got nil")
		}
		if errors.Is(err, sandbox.ErrStalled) {
			t.Errorf("caller cancellation misclassified as ErrStalled: %v", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("caller cancellation err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("caller cancellation during handshake took %s — raw connection did not observe context", elapsed)
		}
	})

	// Once an untimed exec stream is open, going silent mid-stream fails with
	// ErrStalled within the budget (and on K8s, SPDY keepalive pings must not
	// hold the guard open).
	t.Run("WedgedExecStreamFailsWithErrStalled", func(t *testing.T) {
		sb := b.WedgedExecStream(t, budget)
		start := time.Now()
		err := failWithin(t, 5*time.Second, func() error {
			_, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "echo hi"})
			return err
		})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged exec stream err = %v, want sandbox.ErrStalled", err)
		}
		if elapsed := time.Since(start); elapsed < budget {
			t.Errorf("failed in %s, before its %s budget", elapsed, budget)
		}
	})

	// A file read that stalls mid-transfer fails with ErrStalled rather than
	// hanging until the outer work-item stall budget expires.
	t.Run("WedgedReadFileFailsWithErrStalled", func(t *testing.T) {
		sb := b.WedgedReadFile(t, budget)
		start := time.Now()
		err := failWithin(t, 5*time.Second, func() error {
			_, err := sb.ReadFile(context.Background(), "/workspace/a.txt")
			return err
		})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged ReadFile err = %v, want sandbox.ErrStalled", err)
		}
		if elapsed := time.Since(start); elapsed < budget {
			t.Errorf("failed in %s, before its %s budget", elapsed, budget)
		}
	})

	// ReadFileStream carries the same silence bound on the returned stream (or
	// during its buffered read on Kubernetes).
	t.Run("WedgedReadFileStreamFailsWithErrStalled", func(t *testing.T) {
		sb := b.WedgedReadFile(t, budget)
		start := time.Now()
		err := failWithin(t, 5*time.Second, func() error {
			rc, _, err := sb.ReadFileStream(context.Background(), "/workspace/a.txt", sandbox.MaxFileBytes)
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.ReadAll(rc)
			return err
		})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged ReadFileStream err = %v, want sandbox.ErrStalled", err)
		}
		if elapsed := time.Since(start); elapsed < budget {
			t.Errorf("failed in %s, before its %s budget", elapsed, budget)
		}
	})

	// A file write whose peer consumes the payload and then never responds
	// (#318's failure shape) fails with ErrStalled within the budget.
	t.Run("WedgedWriteFileFailsWithErrStalled", func(t *testing.T) {
		sb := b.WedgedWriteFile(t, budget)
		start := time.Now()
		err := failWithin(t, 5*time.Second, func() error {
			return sb.WriteFile(context.Background(), "/workspace/a.txt", []byte("hello"))
		})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("wedged WriteFile err = %v, want sandbox.ErrStalled", err)
		}
		if elapsed := time.Since(start); elapsed < budget {
			t.Errorf("failed in %s, before its %s budget", elapsed, budget)
		}
	})
}

// runLiveStallContract runs the live-sandbox silence-bound contract rows inside
// sandboxtest.Run against real containers/pods.
func runLiveStallContract(t *testing.T, newHarness func(t *testing.T) Harness) {
	t.Helper()

	attachWithBudget := func(t *testing.T, budget time.Duration) (sandbox.Sandbox, sandbox.Sandbox) {
		t.Helper()
		h := newHarness(t)
		if h.WithStallTimeout == nil {
			t.Fatal("Harness.WithStallTimeout is nil: every sandbox backend must configure a silence bound (#395)")
		}
		sid := domain.NewID("sesn")
		ctx := context.Background()
		base, err := h.Provider.Provision(ctx, sandbox.Spec{
			SessionID:  sid,
			Image:      h.Image,
			Workdir:    workdir,
			Networking: domain.Networking{Type: domain.NetUnrestricted},
		})
		if err != nil {
			t.Fatalf("provision: %v", err)
		}
		t.Cleanup(func() {
			if err := base.Destroy(context.Background()); err != nil {
				t.Errorf("destroy: %v", err)
			}
		})
		guarded, err := h.WithStallTimeout(t, budget).Attach(ctx, sid)
		if err != nil {
			t.Fatalf("attach with stall budget %s: %v", budget, err)
		}
		return guarded, base
	}

	// An untimed command that sits completely silent fails with ErrStalled
	// within the configured silence budget rather than hanging forever.
	t.Run("SilentUntimedExecFailsWithErrStalled", func(t *testing.T) {
		const budget = 1500 * time.Millisecond
		sb, _ := attachWithBudget(t, budget)
		start := time.Now()
		err := failWithin(t, 15*time.Second, func() error {
			_, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "sleep 300"})
			return err
		})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("silent untimed exec err = %v, want sandbox.ErrStalled", err)
		}
		if elapsed := time.Since(start); elapsed < budget {
			t.Errorf("failed in %s, inside its %s budget", elapsed, budget)
		}
	})

	// An untimed command that keeps emitting output outlives multiple stall
	// budgets: the guard bounds silence, not wall-clock duration.
	t.Run("ActiveUntimedExecOutlivesStallBudget", func(t *testing.T) {
		const budget = 1500 * time.Millisecond
		sb, _ := attachWithBudget(t, budget)
		start := time.Now()
		var res sandbox.ExecResult
		err := failWithin(t, 20*time.Second, func() error {
			var err error
			res, err = sb.Exec(context.Background(), sandbox.ExecRequest{
				Command: "for i in 1 2 3 4 5 6 7 8 9 10; do echo tick; sleep 0.35; done",
			})
			return err
		})
		if err != nil {
			t.Fatalf("active untimed exec failed: %v", err)
		}
		if res.ExitCode != 0 || strings.Count(res.Stdout, "tick\n") != 10 {
			t.Errorf("unexpected result: %+v", res)
		}
		if elapsed := time.Since(start); elapsed < 2*budget {
			t.Errorf("completed in %s, under 2×%s budget — did not exercise budget resets", elapsed, budget)
		}
	})

	// A command with an explicit ExecRequest.Timeout may legitimately produce
	// zero output for minutes (e.g., bash up to toolset.MaxTimeout = 10m); the
	// transport silence guard must not kill it before its own timeout.
	t.Run("TimedSilentExecOutlivesStallBudget", func(t *testing.T) {
		const budget = 1200 * time.Millisecond
		sb, _ := attachWithBudget(t, budget)
		start := time.Now()
		var res sandbox.ExecResult
		err := failWithin(t, 20*time.Second, func() error {
			var err error
			res, err = sb.Exec(context.Background(), sandbox.ExecRequest{
				Command: "sleep 3; echo done",
				Timeout: 30 * time.Second,
			})
			return err
		})
		if err != nil {
			t.Fatalf("timed silent exec failed: %v", err)
		}
		if res.ExitCode != 0 || res.TimedOut || res.Stdout != "done\n" {
			t.Errorf("unexpected result: %+v", res)
		}
		if elapsed := time.Since(start); elapsed < 2*budget {
			t.Errorf("completed in %s, under 2×%s budget", elapsed, budget)
		}
	})

	// A streaming WriteFileStream that keeps moving bytes outlives multiple
	// stall budgets on both Docker (PUT /archive) and Kubernetes (SPDY stdin).
	t.Run("WriteFileStreamOutlivesStallBudgetWhileStreaming", func(t *testing.T) {
		const budget = 1200 * time.Millisecond
		sb, base := attachWithBudget(t, budget)
		const chunks = 10
		const chunkSize = 4096
		payload := bytes.Repeat([]byte("a"), chunks*chunkSize)
		src := &trickleReader{
			data:      payload,
			chunkSize: chunkSize,
			interval:  300 * time.Millisecond,
		}
		start := time.Now()
		err := failWithin(t, 20*time.Second, func() error {
			return sb.WriteFileStream(context.Background(), workdir+"/slow-upload.bin", src, int64(len(payload)))
		})
		if err != nil {
			t.Fatalf("streaming WriteFileStream failed: %v", err)
		}
		if elapsed := time.Since(start); elapsed < 2*budget {
			t.Errorf("completed in %s, under 2×%s budget — did not exercise upload progress resets", elapsed, budget)
		}
		got, err := base.ReadFile(context.Background(), workdir+"/slow-upload.bin")
		if err != nil {
			t.Fatalf("ReadFile after streaming write: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("read back %d bytes, want %d matching bytes", len(got), len(payload))
		}
	})

	// A WriteFileStream whose source stops delivering bytes fails with
	// ErrStalled within the budget and leaves no partial file at the target.
	t.Run("WriteFileStreamStalledSourceFailsWithErrStalled", func(t *testing.T) {
		const budget = 1200 * time.Millisecond
		sb, base := attachWithBudget(t, budget)
		unblock := make(chan struct{})
		t.Cleanup(func() { close(unblock) })
		src := &stallingReader{
			first:   bytes.Repeat([]byte("z"), 4096),
			unblock: unblock,
		}
		start := time.Now()
		err := failWithin(t, 15*time.Second, func() error {
			return sb.WriteFileStream(context.Background(), workdir+"/stalled-upload.bin", src, 64<<10)
		})
		if !errors.Is(err, sandbox.ErrStalled) {
			t.Fatalf("stalled WriteFileStream err = %v, want sandbox.ErrStalled", err)
		}
		if elapsed := time.Since(start); elapsed < budget {
			t.Errorf("failed in %s, inside its %s budget", elapsed, budget)
		}
		if _, err := base.ReadFile(context.Background(), workdir+"/stalled-upload.bin"); !errors.Is(err, sandbox.ErrFileNotExist) {
			t.Errorf("target after stalled WriteFileStream: err = %v, want ErrFileNotExist", err)
		}
	})
}

type trickleReader struct {
	data      []byte
	offset    int
	chunkSize int
	interval  time.Duration
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	if r.offset > 0 {
		timer := time.NewTimer(r.interval)
		<-timer.C
	}
	end := min(r.offset+r.chunkSize, len(r.data))
	n := copy(p, r.data[r.offset:end])
	r.offset += n
	return n, nil
}

type stallingReader struct {
	first   []byte
	sent    bool
	unblock <-chan struct{}
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.first), nil
	}
	<-r.unblock
	return 0, io.EOF
}

// failWithin runs op in the background and fails the test if it does not return
// within limit. A missing or broken stall bound would otherwise wedge the
// entire test binary until go test's package timeout.
func failWithin(t *testing.T, limit time.Duration, op func() error) error {
	t.Helper()
	res := make(chan error, 1)
	go func() { res <- op() }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case err := <-res:
		return err
	case <-timer.C:
		t.Fatalf("sandbox call did not return within %s — transport silence bound did not fire (#395)", limit)
		return nil
	}
}
