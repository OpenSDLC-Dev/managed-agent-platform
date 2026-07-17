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

func (f *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f.handlers {
		// Required, not defensive: slog's Handler contract says Handle "will
		// only be called when Enabled returns true", and our own Enabled is an
		// OR across the branches — so a level one branch wants and another
		// does not still reaches here.
		if !h.Enabled(ctx, r.Level) {
			continue
		}
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
		slog.NewTextHandler(consoleOut, nil),
		otelslog.NewHandler(serviceName, otelslog.WithLoggerProvider(lp)),
	}}

	prevOut, prevFlags := stdlog.Writer(), stdlog.Flags()
	slog.SetDefault(slog.New(handler))
	stdlog.SetOutput(prevOut)
	stdlog.SetFlags(prevFlags)
}
