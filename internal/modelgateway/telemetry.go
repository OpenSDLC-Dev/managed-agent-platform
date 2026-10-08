package modelgateway

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/catalog"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// instrumentation is the gateway's OTel scope, its tracer's and its meter's.
const instrumentation = "github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"

// The gateway's metrics (docs/plan/59_model-gateway.md, "Telemetry, errors,
// security") take its own names rather than OTel's gen_ai.* ones. Those
// describe one call to a GenAI provider and take the requested model as a
// dimension; these describe a request as the gateway served it, by the alias
// it matched — the configured one, "*" for a wildcard match, so a caller
// choosing names cannot grow a metric — and count the four kinds of token
// the ledger does, where gen_ai.token.type has only input and output. They
// are recorded from the usage each ledger row is written from, once its
// write returns (observe): a row the ledger refuses still counts, without a
// cost. The names are exported so the telemetry contract test can assert
// they reach an OTLP collector.
const (
	MetricRequests         = "modelgateway.requests"
	MetricRequestDuration  = "modelgateway.request.duration"
	MetricTimeToFirstToken = "modelgateway.time_to_first_token"
	MetricTokens           = "modelgateway.tokens"
	MetricCost             = "modelgateway.cost"
)

// The histograms' bucket bounds, in seconds, where the SDK's defaults are
// for milliseconds: a request's, doubling from 10ms to past ten minutes for
// the long streams the gateway relays, and a first token's, finer below a
// second.
var (
	durationBounds = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96,
		81.92, 163.84, 327.68, 655.36}
	ttftBounds = []float64{0.001, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
		20, 40, 80}
)

// The gateway's own attribute keys, beside the conventions' where one
// applies.
const (
	attrAlias      = "modelgateway.alias"
	attrDeployment = "modelgateway.deployment.id"
	attrCredential = "modelgateway.credential.id"
	attrKey        = "modelgateway.api_key.id"
	attrEndpoint   = "modelgateway.endpoint"
	attrTokenType  = "modelgateway.token.type"
)

// operations names the GenAI operation of each route, by its ledger name.
var operations = map[string]string{"messages": "chat", "count_tokens": "count_tokens", "chat_completions": "chat"}

// serverSpan continues the caller's W3C trace context in one server span for
// the request, as the control plane's withTracing does. With no tracer
// provider installed it records nothing.
func serverSpan(r *http.Request) (context.Context, trace.Span) {
	ctx := telemetry.Extract(r.Context(), map[string]string{
		"traceparent": r.Header.Get("traceparent"), "tracestate": r.Header.Get("tracestate")})
	return otel.GetTracerProvider().Tracer(instrumentation).Start(ctx, spanName(r),
		trace.WithSpanKind(trace.SpanKindServer))
}

// spanName is a request span's name, before the request is authenticated:
// its method and route where the gateway serves one, the method alone
// otherwise, and "HTTP" for a method HTTP does not define (OTel's HTTP
// conventions), so no caller can grow the set of span names.
func spanName(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
	default:
		return "HTTP"
	}
	path := strings.TrimPrefix(r.URL.Path, "/anthropic")
	if p, ok := strings.CutPrefix(r.URL.Path, "/openai"); ok {
		path = p
	}
	switch {
	case path == "/v1/messages", path == "/v1/messages/count_tokens", path == "/v1/models", path == "/v1/chat/completions":
		return r.Method + " " + path
	case strings.HasPrefix(path, "/v1/models/"):
		return r.Method + " /v1/models/{model_id}"
	case strings.HasPrefix(r.URL.Path, "/admin/"):
		return r.Method + " /admin"
	}
	return r.Method
}

// statusWriter remembers the status a response was given, for the request's
// span: every response the gateway and its admin API write sets one with
// WriteHeader. Unwrap keeps http.ResponseController reaching the connection
// beneath.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// endServerSpan ends the request's span with the status it was answered
// with, every request's — refused, admitted, the admin API's — and marks a
// 5xx an error: the server's failure, where a 4xx is the caller's.
func endServerSpan(span trace.Span, status int) {
	if status != 0 {
		span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	}
	span.End()
}

// markError names on the request's span the error the gateway answers with.
func markError(ctx context.Context, typ string) {
	trace.SpanFromContext(ctx).SetAttributes(semconv.ErrorTypeKey.String(typ))
}

// answered marks an attempt's span with the status its upstream answered,
// so a refusal the gateway reports as another status, a 401 as a 502, keeps
// the upstream's own; an attempt that got no answer has none.
func answered(ctx context.Context, status int) {
	trace.SpanFromContext(ctx).SetAttributes(semconv.HTTPResponseStatusCode(status))
}

// tracedAttempt makes one attempt under a client span of its own, a child of
// the request's: the upstream model, the provider's profile and host, the
// deployment and credential, the status the upstream answered (answered),
// and the error the attempt ended with. The span covers a stream to its end,
// since the attempt relays it.
func (h *handler) tracedAttempt(w http.ResponseWriter, r *http.Request, c call, at catalog.Attempt, strip bool) (*failure, bool) {
	op := operations[endpoints[c.path]]
	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameKey.String(op),
		semconv.GenAIProviderNameKey.String(at.Provider.Profile),
		semconv.GenAIRequestModel(at.Deployment.UpstreamModel),
		attribute.String(attrDeployment, at.Deployment.ID),
		attribute.String(attrCredential, at.Credential.ID),
	}
	if u, err := url.Parse(at.Endpoint); err == nil {
		attrs = append(attrs, semconv.ServerAddress(u.Hostname()))
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
		}
		if p, err := strconv.Atoi(port); err == nil {
			attrs = append(attrs, semconv.ServerPort(p))
		}
	}
	ctx, span := otel.GetTracerProvider().Tracer(instrumentation).Start(r.Context(), op+" "+at.Deployment.UpstreamModel,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	defer span.End()
	f, retry := h.attempt(w, r.WithContext(ctx), c, at, strip)
	errType := c.out.errType
	if f != nil {
		errType = f.errorType()
	}
	if errType != "" {
		span.SetAttributes(semconv.ErrorTypeKey.String(errType))
		span.SetStatus(codes.Error, errType)
	}
	return f, retry
}

// propagate gives the upstream request the attempt's trace context when the
// provider opted in — an in-cluster model server traced by the same
// collector, say — and none otherwise: the W3C headers are the gateway's to
// set, so a provider's configured headers cannot send them either. With no
// tracer installed the gateway records no span, and the caller's context
// passes on as it came.
func propagate(ctx context.Context, p store.Provider, h http.Header) {
	h.Del("traceparent")
	h.Del("tracestate")
	if !p.PropagateTrace {
		return
	}
	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)
	for k, v := range carrier {
		h.Set(k, v)
	}
}

// errorClass is the error type a metric is labeled with: one errorStatus
// names, and "_OTHER" for any other an upstream does, which could otherwise
// grow the metric without bound; spans and the ledger keep the type as
// given. (An upstream's account refusals reach no ledger row by name:
// refusedCredential answers them as an api_error.)
func errorClass(typ string) string {
	if _, known := errorStatus[typ]; known {
		return typ
	}
	return "_OTHER"
}

// observe reports a finished request to its server span and to the
// gateway's metrics, from the usage its ledger row was written with, cost
// being the row's: nil when it has none, was not written, or holds one no
// float64 can. A telemetry failure drops the reading, never the request.
func observe(ctx context.Context, u store.Usage, at *catalog.Attempt, cost *float64) {
	span := trace.SpanFromContext(ctx)
	dims := []attribute.KeyValue{attribute.String(attrAlias, u.Alias), attribute.String(attrKey, u.APIKeyID),
		attribute.String(attrEndpoint, u.Endpoint)}
	if at != nil {
		dims = append(dims, attribute.String(attrDeployment, at.Deployment.ID))
		// at is the attempt that answered or, where none did, the last one
		// made: a model is named as the response's only for an answer.
		if u.Status < 400 {
			span.SetAttributes(semconv.GenAIResponseModel(at.Deployment.UpstreamModel))
		}
	}
	outcome := []attribute.KeyValue{semconv.HTTPResponseStatusCode(u.Status)}
	if u.ErrorType != "" {
		outcome = append(outcome, semconv.ErrorTypeKey.String(errorClass(u.ErrorType)))
		span.SetAttributes(semconv.ErrorTypeKey.String(u.ErrorType))
		// A stream that failed after its 200 is the server's failure too;
		// endServerSpan marks a 5xx.
		if u.Status < 400 {
			span.SetStatus(codes.Error, u.ErrorType)
		}
	}
	span.SetAttributes(semconv.GenAIOperationNameKey.String(operations[u.Endpoint]), semconv.GenAIRequestModel(u.Model))
	span.SetAttributes(dims...)
	if t := u.Tokens; t != nil {
		// The conventions' input tokens include the cached ones, which
		// Anthropic's input_tokens does not.
		// The keys take int64, which a count past 2^31 needs on a 32-bit build.
		span.SetAttributes(semconv.GenAIUsageInputTokensKey.Int64(t.Input+t.CacheWrite+t.CacheRead),
			semconv.GenAIUsageOutputTokensKey.Int64(t.Output),
			semconv.GenAIUsageCacheCreationInputTokensKey.Int64(t.CacheWrite), semconv.GenAIUsageCacheReadInputTokensKey.Int64(t.CacheRead))
	}

	meter := otel.GetMeterProvider().Meter(instrumentation)
	served := metric.WithAttributes(append(append([]attribute.KeyValue{}, dims...), outcome...)...)
	if c, err := meter.Int64Counter(MetricRequests, metric.WithUnit("{request}"),
		metric.WithDescription("Requests a key's limits admitted, by matched alias, deployment and key.")); err == nil {
		c.Add(ctx, 1, served)
	}
	if hist, err := meter.Float64Histogram(MetricRequestDuration, metric.WithUnit("s"),
		metric.WithDescription("From a request's arrival to its last byte."),
		metric.WithExplicitBucketBoundaries(durationBounds...)); err == nil {
		hist.Record(ctx, u.Latency.Seconds(), served)
	}
	if u.TTFT > 0 {
		if hist, err := meter.Float64Histogram(MetricTimeToFirstToken, metric.WithUnit("s"),
			metric.WithDescription("From a stream's arrival to its first event that is not a keep-alive."),
			metric.WithExplicitBucketBoundaries(ttftBounds...)); err == nil {
			hist.Record(ctx, u.TTFT.Seconds(), metric.WithAttributes(dims...))
		}
	}
	t := u.Tokens
	if t == nil {
		return
	}
	if c, err := meter.Int64Counter(MetricTokens, metric.WithUnit("{token}"),
		metric.WithDescription("Tokens the upstream reported, by kind: input, output, cache_write, cache_read.")); err == nil {
		for _, k := range []struct {
			typ string
			n   int64
		}{{"input", t.Input}, {"output", t.Output}, {"cache_write", t.CacheWrite}, {"cache_read", t.CacheRead}} {
			c.Add(ctx, k.n, metric.WithAttributes(append(append([]attribute.KeyValue{}, dims...), attribute.String(attrTokenType, k.typ))...))
		}
	}
	if cost == nil {
		return
	}
	if c, err := meter.Float64Counter(MetricCost, metric.WithUnit("{cost}"),
		metric.WithDescription("The tokens' cost as the ledger wrote it, at the deployment's prices, in the currency those are entered in.")); err == nil {
		c.Add(ctx, *cost, metric.WithAttributes(dims...))
	}
}
