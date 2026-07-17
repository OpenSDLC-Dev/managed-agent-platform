package telemetry_test

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
)

// restoreLogging saves and restores every global Init touches, so one test's
// bridge does not leak into the next (the suite calls Init repeatedly, and a
// leaked handler points at a torn-down collector).
func restoreLogging(t *testing.T) {
	t.Helper()
	prevSlog := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
}

// console redirects the bridge's console half into a buffer for the duration of
// the test.
func console(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	telemetry.SetConsoleWriterForTest(t, buf)
	return buf
}

func TestInitBridgesSlogToTheCollector(t *testing.T) {
	restoreLogging(t)
	console(t)
	collector := startFakeCollector(t)
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "log-bridge-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	slog.ErrorContext(ctx, "tool_exec item faulted", "work", "work_123")

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	records := collector.logRecords()
	i := slices.IndexFunc(records, func(r logRecord) bool { return r.body == "tool_exec item faulted" })
	if i < 0 {
		t.Fatalf("collector log bodies = %v, want to contain the emitted record", collector.logBodies())
	}
	got := records[i]
	if got.severity != "ERROR" {
		t.Errorf("severity = %q, want %q", got.severity, "ERROR")
	}
	if got.attrs["work"] != "work_123" {
		t.Errorf("attrs[work] = %q, want %q (slog attrs must survive the bridge)", got.attrs["work"], "work_123")
	}
}

// The bridge must not cost us the console: a developer tailing stderr, and
// every existing deployment's log scraping, still see every line.
func TestBridgedLogsStillReachTheConsole(t *testing.T) {
	restoreLogging(t)
	buf := console(t)
	collector := startFakeCollector(t)
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "console-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	slog.InfoContext(ctx, "brain running", "providers", "/etc/providers.json")

	if out := buf.String(); !strings.Contains(out, "brain running") || !strings.Contains(out, "providers=/etc/providers.json") {
		t.Errorf("console output = %q, want the record with its attrs", out)
	}
}

// A log line an operator opens a trace to find has to land *in* that trace.
func TestBridgedLogsCarryTraceCorrelation(t *testing.T) {
	restoreLogging(t)
	console(t)
	collector := startFakeCollector(t)
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "correlation-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	spanCtx, span := otel.Tracer("contract").Start(ctx, "tool_exec")
	slog.ErrorContext(spanCtx, "correlated failure")
	span.End()

	wantTrace := span.SpanContext().TraceID().String()
	wantSpan := span.SpanContext().SpanID().String()

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	records := collector.logRecords()
	i := slices.IndexFunc(records, func(r logRecord) bool { return r.body == "correlated failure" })
	if i < 0 {
		t.Fatalf("collector log bodies = %v, want the correlated record", collector.logBodies())
	}
	if got := records[i]; got.traceID != wantTrace || got.spanID != wantSpan {
		t.Errorf("record trace/span = %s/%s, want %s/%s", got.traceID, got.spanID, wantTrace, wantSpan)
	}
}

// A log call with no span in its context is normal (process startup, the poll
// loop) and must still export — just uncorrelated, rather than dropped.
func TestUncorrelatedLogsStillExport(t *testing.T) {
	restoreLogging(t)
	console(t)
	collector := startFakeCollector(t)
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "uncorrelated-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	slog.Info("worker running", "environment", "env_1")

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if bodies := collector.logBodies(); !slices.Contains(bodies, "worker running") {
		t.Errorf("collector log bodies = %v, want the uncorrelated record exported anyway", bodies)
	}
}

// The regression this whole file exists to prevent.
//
// slog.SetDefault also reroutes the standard library's log package into the
// installed handler (log/slog/logger.go: the *defaultHandler type check, then
// log.SetOutput(&handlerWriter{...})). OTel's own error handler reports export
// failures with log.Print when no delegate is set
// (otel/internal/errorhandler: ErrDelegator.Handle). Wire those together and a
// single failing export becomes self-sustaining: export fails -> log.Print ->
// slog handler -> otelslog -> enqueue a record -> export fails -> ... A
// collector that takes traces but not logs (Jaeger answers Unimplemented on
// logs) drives it at roughly one round per export cycle, forever.
//
// So Init must leave the stdlib log package pointing where it found it.
func TestInitDoesNotRerouteStdlibLog(t *testing.T) {
	restoreLogging(t)
	console(t)
	collector := startFakeCollector(t)
	ctx := context.Background()

	stdlog := &bytes.Buffer{}
	log.SetOutput(stdlog)
	log.SetFlags(log.LstdFlags)

	shutdown, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName: "loop-guard-test",
		Endpoint:    collector.addr,
		Insecure:    true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	if got := log.Writer(); got != stdlog {
		t.Errorf("log.Writer() was rerouted by Init: this is the export-failure feedback loop")
	}
	if got := log.Flags(); got != log.LstdFlags {
		t.Errorf("log.Flags() = %d, want the %d Init found", got, log.LstdFlags)
	}

	log.Print("otel export failed")

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if out := stdlog.String(); !strings.Contains(out, "otel export failed") {
		t.Errorf("stdlib log output = %q, want the line delivered to its own writer", out)
	}
	if bodies := collector.logBodies(); slices.Contains(bodies, "otel export failed") {
		t.Errorf("a stdlib log.Print reached the OTLP bridge: %v — the feedback loop is live", bodies)
	}
}

func TestInitWithoutEndpointLeavesLoggingAlone(t *testing.T) {
	restoreLogging(t)
	before := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()

	shutdown, err := telemetry.Init(context.Background(), telemetry.Config{ServiceName: "test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if slog.Default() != before {
		t.Errorf("disabled Init must not replace the default slog logger")
	}
	if log.Writer() != prevOut || log.Flags() != prevFlags {
		t.Errorf("disabled Init must not touch the stdlib log package")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown: %v", err)
	}
}

// Init reports a failure rather than half-installing. The endpoint here is
// deliberately one that fails while *building an exporter* rather than in
// config validation — validation returns before any of the bridge's code runs,
// so a test driving that path would pass no matter what installLogBridge did.
func TestInitLeavesLoggingUntouchedOnError(t *testing.T) {
	restoreLogging(t)
	before := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()

	_, err := telemetry.Init(context.Background(), telemetry.Config{
		ServiceName: "t",
		Endpoint:    "http://%%%", // survives validation, fails in the exporter's URL parse
		Insecure:    true,
	})
	if err == nil {
		t.Fatal("Init with an unparseable endpoint = nil error, want error")
	}
	if slog.Default() != before {
		t.Errorf("a failed Init replaced the default slog logger")
	}
	if log.Writer() != prevOut || log.Flags() != prevFlags {
		t.Errorf("a failed Init left the stdlib log package rerouted")
	}
}
