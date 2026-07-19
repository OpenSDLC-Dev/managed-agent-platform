package telemetry_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

// The regression this file exists for: the fatal-exit log used to be emitted by
// main() after run() returned, which is after the deferred telemetry shutdown
// had already stopped the log processor. sdk/log's BatchProcessor.OnEmit
// returns without enqueueing once stopped, so the one record that says why the
// process died was the only one the collector never saw — it reached stderr and
// nothing else. Run owns the whole sequence precisely so no caller can put a
// log after the flush again.
func TestRunExportsTheFatalExitLog(t *testing.T) {
	restoreLogging(t)
	console(t)
	collector := startFakeCollector(t)

	ok := telemetry.Run(context.Background(), telemetry.Config{
		ServiceName: "fatal-exit-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	}, func(context.Context) error { return errors.New("DATABASE_URL is required") })

	if ok {
		t.Errorf("Run() = true, want false for a body that returned an error")
	}
	records := collector.logRecords()
	i := slices.IndexFunc(records, func(r logRecord) bool { return r.body == "fatal-exit-test exiting" })
	if i < 0 {
		t.Fatalf("collector log bodies = %v, want to contain the fatal-exit record", collector.logBodies())
	}
	got := records[i]
	if got.severity != "ERROR" {
		t.Errorf("severity = %q, want %q", got.severity, "ERROR")
	}
	// otelslog renders an error-valued attr under the semantic-convention
	// exception.* keys rather than keeping slog's own key, so the reason is
	// asserted where a backend actually shows it.
	if got.attrs["exception.message"] != "DATABASE_URL is required" {
		t.Errorf("attrs[exception.message] = %q, want the body's error text", got.attrs["exception.message"])
	}
}

// The collector must not cost the operator the stderr line: whoever is reading
// `kubectl logs` still learns why the process died.
func TestRunFatalExitLogAlsoReachesTheConsole(t *testing.T) {
	restoreLogging(t)
	buf := console(t)
	collector := startFakeCollector(t)

	telemetry.Run(context.Background(), telemetry.Config{
		ServiceName: "console-exit-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	}, func(context.Context) error { return errors.New("store.Open failed") })

	if out := buf.String(); !strings.Contains(out, "console-exit-test exiting") || !strings.Contains(out, "store.Open failed") {
		t.Errorf("console = %q, want the fatal-exit line", out)
	}
}

// The flush runs on its own context.Background(), and this is the test that
// says so. On a signal-driven exit the process context is already cancelled by
// the time the body returns, and sdk/log's BatchProcessor.Shutdown skips its
// final queue flush outright when the shutdown context is done — so deriving
// the flush context from ctx puts the fatal record back exactly where #93 had
// it: printed on the console, never exported. Every other test here passes a
// context.Background() that is never cancelled, so that mutation survives all
// of them.
func TestRunFlushesTheFatalLogEvenWhenTheProcessContextIsCancelled(t *testing.T) {
	restoreLogging(t)
	console(t)
	collector := startFakeCollector(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ok := telemetry.Run(ctx, telemetry.Config{
		ServiceName: "sigterm-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	}, func(context.Context) error {
		cancel() // the signal lands, then the body fails on the way down
		return errors.New("store: pool closed unexpectedly")
	})

	if ok {
		t.Errorf("Run() = true, want false for a body that returned an error")
	}
	if bodies := collector.logBodies(); !slices.Contains(bodies, "sigterm-test exiting") {
		t.Fatalf("collector log bodies = %v, want the fatal-exit record despite the cancelled process context", bodies)
	}
}

// The fatal record is the last thing queued before the flush, so it is the
// first thing a shared shutdown deadline can starve. All three providers shut
// down on one deadline, and a meter provider exports unconditionally at
// Shutdown even for a service that recorded nothing — so a collector that takes
// logs but stalls on metrics would, with logs draining last, spend the whole
// budget and leave BatchProcessor.Shutdown to return on ctx.Done without ever
// flushing its queue. Logs drain first for exactly this reason.
func TestRunFlushesTheFatalLogAheadOfSlowerTelemetry(t *testing.T) {
	restoreLogging(t)
	console(t)
	// Park the BatchProcessor's background poller past the end of the test, so
	// the shutdown flush is the only thing that can deliver the record. Left at
	// its 1s default the poller ships it during the stall on its own, and the
	// shutdown order this test exists to pin makes no difference to the result.
	t.Setenv("OTEL_BLRP_SCHEDULE_DELAY", "600000")
	collector := startFakeCollector(t)
	collector.stallMetrics(t)

	done := make(chan bool, 1)
	go func() {
		done <- telemetry.Run(context.Background(), telemetry.Config{
			ServiceName: "stalled-metrics-test",
			Endpoint:    collector.addr,
			Insecure:    true,
		}, func(context.Context) error { return errors.New("store: pool closed unexpectedly") })
	}()

	select {
	case ok := <-done:
		if ok {
			t.Errorf("Run() = true, want false for a body that returned an error")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return; the flush is not bounded")
	}
	if bodies := collector.logBodies(); !slices.Contains(bodies, "stalled-metrics-test exiting") {
		t.Fatalf("collector log bodies = %v, want the fatal-exit record despite metrics stalling the shutdown", bodies)
	}
}

// A signal-cancelled shutdown is how these processes are meant to stop, so it
// is neither a fatal error to log nor a non-zero exit. The predicate lives here
// alone — a caller that re-derived it could drift from what Run logs.
func TestRunTreatsContextCanceledAsACleanExit(t *testing.T) {
	restoreLogging(t)
	console(t)
	collector := startFakeCollector(t)

	ok := telemetry.Run(context.Background(), telemetry.Config{
		ServiceName: "canceled-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	}, func(context.Context) error { return fmt.Errorf("worker loop: %w", context.Canceled) })

	if !ok {
		t.Errorf("Run() = false, want true for a context.Canceled body error")
	}
	if bodies := collector.logBodies(); slices.Contains(bodies, "canceled-test exiting") {
		t.Errorf("collector log bodies = %v, want no fatal-exit record for a clean shutdown", bodies)
	}
}

func TestRunReportsACleanExitForANilError(t *testing.T) {
	restoreLogging(t)
	console(t)
	collector := startFakeCollector(t)

	ran := false
	ok := telemetry.Run(context.Background(), telemetry.Config{
		ServiceName: "clean-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	}, func(context.Context) error { ran = true; return nil })

	if !ok {
		t.Errorf("Run() = false, want true for a body that returned nil")
	}
	if !ran {
		t.Errorf("Run did not call body")
	}
	if bodies := collector.logBodies(); slices.Contains(bodies, "clean-test exiting") {
		t.Errorf("collector log bodies = %v, want no fatal-exit record", bodies)
	}
}

// Telemetry failing to start is the one error that predates the bridge it would
// have been exported through, so it can only reach stderr. It must still stop
// the process rather than run the body without telemetry.
//
// The console seam is no use here — it belongs to the bridge, and on this path
// Init returns before installing one — so the assertion goes through the
// default logger, which also keeps the line out of the suite's own stderr.
func TestRunReportsAFailedTelemetryInitWithoutRunningTheBody(t *testing.T) {
	restoreLogging(t)
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))

	ran := false
	ok := telemetry.Run(context.Background(), telemetry.Config{
		ServiceName: "", // Init rejects an empty service name.
	}, func(context.Context) error { ran = true; return nil })

	if ok {
		t.Errorf("Run() = true, want false when telemetry.Init fails")
	}
	if ran {
		t.Errorf("Run called body after telemetry.Init failed")
	}
	if out := buf.String(); !strings.Contains(out, "telemetry: ServiceName is required") {
		t.Errorf("log = %q, want Init's error reported before the process gives up", out)
	}
}

// Init installs no bridge without an endpoint, so Run must stay a plain
// call-the-body wrapper there — a deployment with no collector is the default.
func TestRunWorksWithoutAnEndpoint(t *testing.T) {
	restoreLogging(t)

	ok := telemetry.Run(context.Background(), telemetry.Config{
		ServiceName: "offline-test",
	}, func(context.Context) error { return nil })

	if !ok {
		t.Errorf("Run() = false, want true with no endpoint configured")
	}
}
