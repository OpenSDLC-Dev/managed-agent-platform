package telemetry

import (
	"context"
	"errors"
	"io"
	stdlog "log"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// consoleOut is where the bridge's console half writes. A variable only so a
// test can read what was written; production never reassigns it.
var consoleOut io.Writer = os.Stderr

// bridgeLevel is the floor the fan-out keeps, and it is deliberately the floor
// the process already had: slog's default handler logs at Info and above
// (log/slog's logLoggerLevel, which nothing here moves), so before the bridge a
// slog.Debug was dropped at the Enabled check and never existed at all.
//
// A const, not a Config field, because nothing asks for another value: there is
// no Debug call site in the tree and no operator switch for one. It does close
// slog.SetLogLoggerLevel as an escape hatch once the bridge is installed —
// nothing uses that either, and a knob that ships records off the machine is
// worth adding deliberately, with a config field and a test, rather than
// inheriting by accident.
const bridgeLevel = slog.LevelInfo

// fanoutHandler writes every record to each handler in turn: the console keeps
// working exactly as before the bridge existed, and the collector gets the same
// record. No branch may short-circuit another — a failing OTLP export must never
// cost the operator the stderr line, which is the one that still works when the
// collector is the thing that is broken. (slog.Logger discards Handle's error,
// so the returned join is for a wrapping handler's benefit, not ours; what
// matters is that a branch's failure does not skip the branches after it.)
type fanoutHandler struct {
	handlers []slog.Handler
}

// Enabled answers for the fan-out as a whole rather than ORing its branches.
// The OTLP branch has no floor of its own — sdk/log's BatchProcessor.Enabled
// returns true unconditionally — so an OR would answer true for Debug and
// installing the bridge would silently widen what the process logs: Debug
// records would ship to the collector while the console, still at Info, showed
// nothing. Adding an endpoint must change where records go, never which records
// exist.
func (f *fanoutHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= bridgeLevel
}

// Handle sends the record to every branch, with no per-branch Enabled re-check:
// Enabled above has already answered for all of them. Neither branch can want
// less than it gates — the console's TextHandler is built with bridgeLevel, and
// the OTLP branch accepts everything (otelslog has no level option at all; its
// Enabled delegates to the BatchProcessor's, which is unconditionally true).
func (f *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f.handlers {
		// A Record shares a backing array with its copies; Clone exists so two
		// consumers cannot interfere, which is exactly what a fan-out is.
		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &fanoutHandler{handlers: next}
}

func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(f.handlers))
	for i, h := range f.handlers {
		next[i] = h.WithGroup(name)
	}
	return &fanoutHandler{handlers: next}
}

// installLogBridge points the default slog logger at both the console and lp,
// so the existing slog.*Context call sites export to the collector with no new
// logging API.
//
// The stdlib-log restore is load-bearing, not tidiness. slog.SetDefault also
// reroutes the standard library's log package into the handler it installs
// (log/slog/logger.go: anything that is not a *defaultHandler gets
// log.SetOutput(&handlerWriter{...}) and log.SetFlags(0)). OTel reports its own
// export failures with log.Print when no error-handler delegate is set
// (otel/internal/errorhandler: ErrDelegator.Handle). Left connected, those two
// close a circuit: an export fails, otel log.Prints it, the line enters the
// slog handler, otelslog enqueues it as a record, exporting *that* fails, and
// so on for the life of the process. It is not hypothetical — Jaeger takes
// traces but answers Unimplemented on logs, which is the compose stack's own
// default. Restoring log's writer and flags is what leaves the circuit open.
//
// Wrapping the handler slog already had is the other tempting shape and is
// worse: the default handler writes through log.Output, so once SetDefault has
// pointed log at the handler the two deadlock on log's mutex — the reason for
// the *defaultHandler type check in SetDefault. A TextHandler owns its writer
// and has no such edge; the cost is that the console format changes when an
// endpoint is configured, which the CHANGELOG declares.
func installLogBridge(serviceName string, lp *sdklog.LoggerProvider) {
	handler := &fanoutHandler{handlers: []slog.Handler{
		// bridgeLevel explicitly, though it is also TextHandler's default: the
		// console's floor agreeing with the fan-out's must be a decision, not a
		// coincidence that a later edit could quietly break. The OTLP branch
		// takes no floor — otelslog has no option for one — which is exactly
		// why Enabled above cannot ask its branches and must answer itself.
		slog.NewTextHandler(consoleOut, &slog.HandlerOptions{Level: bridgeLevel}),
		otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(lp)),
	}}

	prevOut, prevFlags := stdlog.Writer(), stdlog.Flags()
	slog.SetDefault(slog.New(handler))
	stdlog.SetOutput(prevOut)
	stdlog.SetFlags(prevFlags)
}
