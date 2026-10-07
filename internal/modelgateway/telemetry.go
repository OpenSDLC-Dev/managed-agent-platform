package modelgateway

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

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
// are recorded with the ledger row (observe), so the two cannot drift. The
// names are exported so the telemetry contract test can assert they reach an
// OTLP collector.
const (
	MetricRequests         = "modelgateway.requests"
	MetricRequestDuration  = "modelgateway.request.duration"
	MetricTimeToFirstToken = "modelgateway.time_to_first_token"
	MetricTokens           = "modelgateway.tokens"
	MetricCost             = "modelgateway.cost"
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
var operations = map[string]string{"messages": "chat", "count_tokens": "count_tokens"}

// serverSpan continues the caller's W3C trace context in one server span for
// the request, as the control plane's withTracing does. With no tracer
// provider installed it records nothing.
func serverSpan(r *http.Request) (context.Context, trace.Span) {
	ctx := telemetry.Extract(r.Context(), map[string]string{
		"traceparent": r.Header.Get("traceparent"), "tracestate": r.Header.Get("tracestate")})
	return otel.GetTracerProvider().Tracer(instrumentation).Start(ctx, r.Method+" "+r.URL.Path,
		trace.WithSpanKind(trace.SpanKindServer))
}

// tracedAttempt makes one attempt under a client span of its own, a child of
// the request's: the upstream model, the provider's profile and host, the
// deployment and credential, and the status and error the attempt ended
// with. The span covers a stream to its end, since the attempt relays it.
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
		if p, err := strconv.Atoi(u.Port()); err == nil {
			attrs = append(attrs, semconv.ServerPort(p))
		}
	}
	ctx, span := otel.GetTracerProvider().Tracer(instrumentation).Start(r.Context(), op+" "+at.Deployment.UpstreamModel,
		trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
	defer span.End()
	f, retry := h.attempt(w, r.WithContext(ctx), c, at, strip)
	status, errType := c.out.status, c.out.errType
	if f != nil {
		status, errType = f.status, f.errorType()
	}
	span.SetAttributes(semconv.HTTPResponseStatusCode(status))
	if errType != "" {
		span.SetAttributes(semconv.ErrorTypeKey.String(errType))
		span.SetStatus(codes.Error, errType)
	}
	return f, retry
}

// propagate sends the attempt's trace context upstream, to a provider that
// opted in: an in-cluster model server traced by the same collector, say.
func propagate(ctx context.Context, p store.Provider, h http.Header) {
	if !p.PropagateTrace {
		return
	}
	carrier := map[string]string{}
	telemetry.Inject(ctx, carrier)
	for k, v := range carrier {
		h.Set(k, v)
	}
}

// observe reports a finished request to its server span and to the
// gateway's metrics, from the usage its ledger row is written from. Its cost
// is at the snapshot's prices, which the ledger's — the database's when the
// row is written — follow within a reload. A telemetry failure drops the
// reading, never the request.
func observe(ctx context.Context, u store.Usage, at *catalog.Attempt) {
	span := trace.SpanFromContext(ctx)
	dims := []attribute.KeyValue{attribute.String(attrAlias, u.Alias), attribute.String(attrKey, u.APIKeyID),
		attribute.String(attrEndpoint, u.Endpoint)}
	if at != nil {
		dims = append(dims, attribute.String(attrDeployment, at.Deployment.ID))
		span.SetAttributes(semconv.GenAIResponseModel(at.Deployment.UpstreamModel))
	}
	outcome := []attribute.KeyValue{semconv.HTTPResponseStatusCode(u.Status)}
	if u.ErrorType != "" {
		outcome = append(outcome, semconv.ErrorTypeKey.String(u.ErrorType))
		// A server span's error is the server's: a 4xx is the caller's, but a
		// stream that failed after its 200 is not.
		if u.Status < 400 || u.Status >= 500 {
			span.SetStatus(codes.Error, u.ErrorType)
		}
	}
	span.SetAttributes(semconv.GenAIOperationNameKey.String(operations[u.Endpoint]), semconv.GenAIRequestModel(u.Model))
	span.SetAttributes(dims...)
	span.SetAttributes(outcome...)
	if t := u.Tokens; t != nil {
		span.SetAttributes(semconv.GenAIUsageInputTokens(int(t.Input)), semconv.GenAIUsageOutputTokens(int(t.Output)),
			semconv.GenAIUsageCacheCreationInputTokens(int(t.CacheWrite)), semconv.GenAIUsageCacheReadInputTokens(int(t.CacheRead)))
	}

	meter := otel.GetMeterProvider().Meter(instrumentation)
	served := metric.WithAttributes(append(append([]attribute.KeyValue{}, dims...), outcome...)...)
	if c, err := meter.Int64Counter(MetricRequests, metric.WithUnit("{request}"),
		metric.WithDescription("Requests a key's limits admitted, by matched alias, deployment and key.")); err == nil {
		c.Add(ctx, 1, served)
	}
	if hist, err := meter.Float64Histogram(MetricRequestDuration, metric.WithUnit("s"),
		metric.WithDescription("From a request's arrival to its last byte.")); err == nil {
		hist.Record(ctx, u.Latency.Seconds(), served)
	}
	if u.TTFT > 0 {
		if hist, err := meter.Float64Histogram(MetricTimeToFirstToken, metric.WithUnit("s"),
			metric.WithDescription("From a stream's arrival to its first event that is not a keep-alive.")); err == nil {
			hist.Record(ctx, u.TTFT.Seconds(), metric.WithAttributes(dims...))
		}
	}
	t := u.Tokens
	if t == nil || at == nil {
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
	if c, err := meter.Float64Counter(MetricCost, metric.WithUnit("{cost}"),
		metric.WithDescription("The tokens' cost at the deployment's prices, in the currency those are entered in.")); err == nil {
		c.Add(ctx, costOf(*t, at.Deployment.Prices), metric.WithAttributes(dims...))
	}
}

// costOf is the tokens' cost at prices per million, a missing price costing
// nothing, as the ledger computes it.
func costOf(t store.Tokens, p store.Prices) float64 {
	price := func(v *float64) float64 {
		if v == nil {
			return 0
		}
		return *v
	}
	return (price(p.Input)*float64(t.Input) + price(p.Output)*float64(t.Output) +
		price(p.CacheWrite)*float64(t.CacheWrite) + price(p.CacheRead)*float64(t.CacheRead)) / 1e6
}
