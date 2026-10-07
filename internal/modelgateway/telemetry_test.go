package modelgateway_test

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway"
	"github.com/OpenSDLC-Dev/managed-agent-platform/internal/modelgateway/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// observed installs a recording tracer provider and a manual-reader meter
// provider for one test and restores the globals after it; the gateway's
// tests run one at a time, so no other test sees them.
func observed(t *testing.T) (*tracetest.SpanRecorder, func() metricdata.ResourceMetrics) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	prevT, prevM := otel.GetTracerProvider(), otel.GetMeterProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetTracerProvider(prevT); otel.SetMeterProvider(prevM) })
	return rec, func() metricdata.ResourceMetrics {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatal(err)
		}
		return rm
	}
}

// ended is every span rec holds of kind, once there are at least n of them.
func ended(t *testing.T, rec *tracetest.SpanRecorder, kind trace.SpanKind, n int) []sdktrace.ReadOnlySpan {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		var out []sdktrace.ReadOnlySpan
		for _, s := range rec.Ended() {
			if s.SpanKind() == kind {
				out = append(out, s)
			}
		}
		if len(out) >= n || time.Now().After(deadline) {
			return out
		}
	}
}

func attr(kvs []attribute.KeyValue, key string) (string, bool) {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value.Emit(), true
		}
	}
	return "", false
}

const (
	callerTrace  = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerParent = "00f067aa0ba902b7"
	traceparent  = "00-" + callerTrace + "-" + callerParent + "-01"
)

// A request is one server span continuing the caller's trace — the name it
// sent, the alias that matched (here the wildcard), the upstream model that
// answered, its status and tokens — and each upstream attempt one client span
// under it: the upstream model, the provider's profile and host, the
// deployment and credential, and the status and error it ended with, a
// failed attempt's span marked an error.
func TestARequestIsTracedAcrossItsAttempts(t *testing.T) {
	rec, _ := observed(t)
	e := newEnv(t)
	down := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		writeBody(w, 500, `{"type":"error","error":{"type":"api_error","message":"down"}}`)
	})
	up := newFake(t, message("ok"))
	pd, pu := e.provider(down.URL), e.provider(up.URL)
	e.credential(pd, "sk-upstream-1", 1)
	cred := e.credential(pu, "sk-upstream-2", 1)
	first, second := e.deployment(pd, "model-a"), e.deployment(pu, "model-b")
	e.alias("*", target(first, 0), target(second, 1))
	key := e.key(everyAlias)
	e.start()
	resp, b := e.do("POST", "/v1/messages", `{"model":"claude-x","max_tokens":8,"messages":[]}`,
		map[string]string{"x-api-key": key, "traceparent": traceparent})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}

	servers := ended(t, rec, trace.SpanKindServer, 1)
	if len(servers) != 1 {
		t.Fatalf("%d server spans, want 1", len(servers))
	}
	s := servers[0]
	if s.SpanContext().TraceID().String() != callerTrace || s.Parent().SpanID().String() != callerParent || !s.Parent().IsRemote() {
		t.Errorf("the server span is %s under %s, want the caller's trace %s under %s",
			s.SpanContext().TraceID(), s.Parent().SpanID(), callerTrace, callerParent)
	}
	for k, want := range map[string]string{
		"gen_ai.request.model": "claude-x", "gen_ai.response.model": "model-b", "modelgateway.alias": "*",
		"modelgateway.deployment.id": second.ID, "http.response.status_code": "200",
		"gen_ai.usage.input_tokens": "5", "gen_ai.usage.output_tokens": "2",
	} {
		if got, _ := attr(s.Attributes(), k); got != want {
			t.Errorf("server span %s = %q, want %q", k, got, want)
		}
	}

	calls := len(down.recorded()) + len(up.recorded())
	clients := ended(t, rec, trace.SpanKindClient, calls)
	if len(clients) != calls || calls < 2 {
		t.Fatalf("%d client spans for %d upstream calls", len(clients), calls)
	}
	if s.Status().Code != codes.Unset {
		t.Errorf("the answered request's span is %v", s.Status())
	}
	for i, c := range clients {
		failed := i < len(clients)-1
		if (c.Status().Code == codes.Error) != failed {
			t.Errorf("client span %d's status is %v; failed %t", i, c.Status(), failed)
		}
		if c.Parent().SpanID() != s.SpanContext().SpanID() {
			t.Errorf("client span %d is under %s, want the server span %s", i, c.Parent().SpanID(), s.SpanContext().SpanID())
		}
		dep, _ := attr(c.Attributes(), "modelgateway.deployment.id")
		want := map[string]string{"gen_ai.operation.name": "chat", "gen_ai.provider.name": "anthropic-generic",
			"server.address": "127.0.0.1", "http.response.status_code": "500", "error.type": "api_error",
			"gen_ai.request.model": "model-a"}
		if i == len(clients)-1 {
			want = map[string]string{"gen_ai.request.model": "model-b", "http.response.status_code": "200",
				"modelgateway.credential.id": cred.ID}
			if dep != second.ID || c.Name() != "chat model-b" {
				t.Errorf("the last client span is %q for %s, want chat model-b for %s", c.Name(), dep, second.ID)
			}
			if _, ok := attr(c.Attributes(), "error.type"); ok {
				t.Error("the answering attempt's span names an error")
			}
		} else if dep != first.ID {
			t.Errorf("client span %d is for %s, want %s", i, dep, first.ID)
		}
		for k, v := range want {
			if got, _ := attr(c.Attributes(), k); got != v {
				t.Errorf("client span %d %s = %q, want %q", i, k, got, v)
			}
		}
	}
}

// The gateway sends trace context upstream only to a provider that opts in,
// and then its own, for the attempt's span, with the caller's tracestate —
// never the caller's traceparent as sent, and never what a provider's
// configured headers name.
func TestTraceContextGoesUpstreamOnlyWhereAProviderOptsIn(t *testing.T) {
	rec, _ := observed(t)
	e := newEnv(t)
	plain, traced := newFake(t, message("ok")), newFake(t, message("ok"))
	forged := map[string]string{"traceparent": "00-" + strings.Repeat("ab", 16) + "-" + strings.Repeat("cd", 8) + "-01",
		"tracestate": "vendor=forged"}
	pp := e.provider(plain.URL, func(p *store.Provider) { p.Headers = forged })
	pt := e.provider(traced.URL, func(p *store.Provider) { p.PropagateTrace, p.Headers = true, forged })
	e.credential(pp, "sk-upstream-1", 1)
	e.credential(pt, "sk-upstream-2", 1)
	e.alias("plain", target(e.deployment(pp, "m"), 0))
	dt := e.deployment(pt, "m")
	e.alias("traced", target(dt, 0))
	key := e.key(everyAlias)
	e.start()
	for _, alias := range []string{"plain", "traced"} {
		if resp, b := e.do("POST", "/v1/messages", `{"model":"`+alias+`","max_tokens":8,"messages":[]}`,
			map[string]string{"x-api-key": key, "traceparent": traceparent, "tracestate": "caller=1"}); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", alias, resp.StatusCode, b)
		}
	}
	for _, h := range []string{"traceparent", "tracestate"} {
		if got := plain.recorded()[0].Header.Get(h); got != "" {
			t.Errorf("a provider that did not opt in was sent %s %q", h, got)
		}
	}
	if got := traced.recorded()[0].Header.Get("tracestate"); got != "caller=1" {
		t.Errorf("the opted-in provider was sent tracestate %q, want the caller's", got)
	}
	var attempt trace.SpanContext
	for _, c := range ended(t, rec, trace.SpanKindClient, 2) {
		if dep, _ := attr(c.Attributes(), "modelgateway.deployment.id"); dep == dt.ID {
			attempt = c.SpanContext()
		}
	}
	want := "00-" + callerTrace + "-" + attempt.SpanID().String() + "-01"
	if got := traced.recorded()[0].Header.Get("traceparent"); got != want || !attempt.IsValid() {
		t.Errorf("the opted-in provider was sent traceparent %q, want %q", got, want)
	}
}

// With no tracer installed the gateway records no span, so a provider that
// opted in is sent the caller's trace context as it came, and one that did
// not is sent none.
func TestWithNoTracerTheCallersContextPassesAsItCame(t *testing.T) {
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	e := newEnv(t)
	plain, traced := newFake(t, message("ok")), newFake(t, message("ok"))
	pp := e.provider(plain.URL)
	pt := e.provider(traced.URL, func(p *store.Provider) { p.PropagateTrace = true })
	e.credential(pp, "sk-upstream-1", 1)
	e.credential(pt, "sk-upstream-2", 1)
	e.alias("plain", target(e.deployment(pp, "m"), 0))
	e.alias("traced", target(e.deployment(pt, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	for _, alias := range []string{"plain", "traced"} {
		if resp, b := e.do("POST", "/v1/messages", `{"model":"`+alias+`","max_tokens":8,"messages":[]}`,
			map[string]string{"x-api-key": key, "traceparent": traceparent}); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", alias, resp.StatusCode, b)
		}
	}
	if got := plain.recorded()[0].Header.Get("traceparent"); got != "" {
		t.Errorf("a provider that did not opt in was sent traceparent %q", got)
	}
	if got := traced.recorded()[0].Header.Get("traceparent"); got != traceparent {
		t.Errorf("the opted-in provider was sent traceparent %q, want the caller's %q", got, traceparent)
	}
}

// Every request's span holds the status it was answered with — refused
// before admission, unservable, the models list — and is an error only where
// the gateway failed it.
func TestEveryAnswerReachesItsSpan(t *testing.T) {
	rec, _ := observed(t)
	e := newEnv(t)
	idle := e.provider("http://127.0.0.1:1") // no credential, so nothing to attempt
	e.alias("idle", target(e.deployment(idle, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	e.do("POST", "/v1/messages", `{"model":"idle","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": "sk-wrong"})
	e.do("POST", "/v1/messages", `{"model":"idle","max_tokens":8,"messages":[]}`, map[string]string{"x-api-key": key})
	e.do("GET", "/v1/models", "", map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"})
	want := map[string]struct {
		errType string
		failed  bool
	}{"401": {"authentication_error", false}, "503": {"api_error", true}, "200": {"", false}}
	spans := ended(t, rec, trace.SpanKindServer, 3)
	if len(spans) != 3 {
		t.Fatalf("%d server spans, want 3", len(spans))
	}
	for _, s := range spans {
		status, _ := attr(s.Attributes(), "http.response.status_code")
		w, ok := want[status]
		if !ok {
			t.Errorf("a server span holds status %q", status)
			continue
		}
		delete(want, status)
		if got, _ := attr(s.Attributes(), "error.type"); got != w.errType {
			t.Errorf("the %s's span names error %q, want %q", status, got, w.errType)
		}
		if (s.Status().Code == codes.Error) != w.failed {
			t.Errorf("the %s's span is %v", status, s.Status())
		}
	}
}

// An attempt's span holds the status its upstream answered, where the
// gateway answers the caller with another — a refused key's 401 is the
// caller's server error — and none where no answer came.
func TestAnAttemptsSpanHoldsWhatItsUpstreamAnswered(t *testing.T) {
	rec, _ := observed(t)
	e := newEnv(t)
	refusing := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		writeBody(w, 401, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
	})
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	pr, pg := e.provider(refusing.URL), e.provider(gone.URL)
	e.credential(pr, "sk-upstream-1", 1)
	e.credential(pg, "sk-upstream-2", 1)
	dr, dg := e.deployment(pr, "refused"), e.deployment(pg, "unreachable")
	e.alias("refused", target(dr, 0))
	e.alias("unreachable", target(dg, 0))
	key := e.key(everyAlias)
	e.start()
	for _, alias := range []string{"refused", "unreachable"} {
		if resp, b := e.do("POST", "/v1/messages", `{"model":"`+alias+`","max_tokens":8,"messages":[]}`,
			map[string]string{"x-api-key": key}); resp.StatusCode < 500 {
			t.Fatalf("%s: %d %s, want a server error", alias, resp.StatusCode, b)
		}
	}
	seen := map[string]bool{}
	for _, c := range ended(t, rec, trace.SpanKindClient, 2) {
		dep, _ := attr(c.Attributes(), "modelgateway.deployment.id")
		status, has := attr(c.Attributes(), "http.response.status_code")
		seen[dep] = true
		switch {
		case dep == dr.ID && status != "401":
			t.Errorf("the refused attempt's span holds status %q, want the upstream's 401", status)
		case dep == dg.ID && has:
			t.Errorf("the unanswered attempt's span holds status %q", status)
		}
		if c.Status().Code != codes.Error {
			t.Errorf("the %s attempt's span is %v", dep, c.Status())
		}
	}
	if !seen[dr.ID] || !seen[dg.ID] {
		t.Errorf("client spans for %v, want both deployments", seen)
	}
}

// metricSum adds up the named counter's points whose attributes include
// match, and counts the named histogram's readings likewise.
func metricSum(rm metricdata.ResourceMetrics, name string, match map[string]string) (sum float64, count uint64) {
	matches := func(set attribute.Set) bool {
		for k, v := range match {
			if got, ok := set.Value(attribute.Key(k)); !ok || got.Emit() != v {
				return false
			}
		}
		return true
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					if matches(p.Attributes) {
						sum += float64(p.Value)
					}
				}
			case metricdata.Sum[float64]:
				for _, p := range d.DataPoints {
					if matches(p.Attributes) {
						sum += p.Value
					}
				}
			case metricdata.Histogram[float64]:
				for _, p := range d.DataPoints {
					if matches(p.Attributes) {
						sum, count = sum+p.Sum, count+p.Count
					}
				}
			}
		}
	}
	return sum, count
}

// The metrics count what the ledger records, by matched alias, deployment and
// key: each request by route, status and error, its latency, a stream's time
// to first token, and the tokens and the ledger's cost of them — at the
// database's prices, here edited since the snapshot loaded. An error type
// outside Anthropic's own is _OTHER there, as given in the ledger and on the
// span. A request's span is an error when the gateway failed it, a stream
// failing after its 200 included, not when the caller's request was refused.
func TestTheMetricsCountWhatTheLedgerRecords(t *testing.T) {
	rec, collect := observed(t)
	e := newEnv(t)
	up := newFake(t, func(w http.ResponseWriter, r *http.Request, c fakeCall) {
		switch {
		case strings.Contains(string(c.Raw), "refuse"):
			writeBody(w, 400, `{"type":"error","error":{"type":"invalid_request_error","message":"no"}}`)
		case strings.Contains(string(c.Raw), "odd"):
			writeBody(w, 400, `{"type":"error","error":{"type":"vendor_specific_error","message":"no"}}`)
		case strings.Contains(string(c.Raw), "midstream"):
			sse(w, events(c.Model, "x")[0], overloadedEvent)
		case strings.Contains(string(c.Raw), "cached"):
			writeBody(w, 200, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn",`+
				`"usage":{"input_tokens":5,"output_tokens":2,"cache_creation_input_tokens":7,"cache_read_input_tokens":11}}`)
		default:
			message("ok")(w, r, c)
		}
	})
	down := newFake(t, func(w http.ResponseWriter, _ *http.Request, _ fakeCall) {
		writeBody(w, 500, `{"type":"error","error":{"type":"api_error","message":"down"}}`)
	})
	p, pd := e.provider(up.URL), e.provider(down.URL)
	e.credential(p, "sk-upstream-1", 1)
	e.credential(pd, "sk-upstream-2", 1)
	d := e.deployment(p, "m", func(d *store.Deployment) {
		d.Prices = store.Prices{Input: priced(3), Output: priced(15), CacheWrite: priced(3.75), CacheRead: priced(0.3)}
	})
	e.alias("fast", target(d, 0))
	e.alias("*", target(d, 0))
	e.alias("broken", target(e.deployment(pd, "m"), 0))
	key := e.key(everyAlias)
	e.start()
	if _, err := e.s.UpdateDeployment(e.ctx, d.ID, func(d *store.Deployment) error {
		d.Prices = store.Prices{Input: priced(6), Output: priced(30), CacheWrite: priced(7.5), CacheRead: priced(0.6)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hdr := map[string]string{"x-api-key": key}
	e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"messages":[]}`, hdr)
	e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"messages":[]}`, hdr)
	e.do("POST", "/v1/messages/count_tokens", `{"model":"fast","messages":[]}`, hdr)
	e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"metadata":{"user_id":"refuse"},"messages":[]}`, hdr)
	e.do("POST", "/v1/messages", `{"model":"any-name-at-all","max_tokens":8,"metadata":{"user_id":"cached"},"messages":[]}`, hdr)
	e.do("POST", "/v1/messages", `{"model":"broken","max_tokens":8,"messages":[]}`, hdr)
	e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"metadata":{"user_id":"odd"},"messages":[]}`, hdr)
	e.do("POST", "/v1/messages", `{"model":"fast","max_tokens":8,"stream":true,"metadata":{"user_id":"midstream"},"messages":[]}`, hdr)
	rows := e.ledger()
	if len(rows) != 8 {
		t.Fatalf("%d ledger rows, want 8", len(rows))
	}
	if rows[1].ErrorType != "vendor_specific_error" {
		t.Errorf("the ledger holds error %q, want the upstream's as given", rows[1].ErrorType)
	}
	var odd bool
	for _, s := range ended(t, rec, trace.SpanKindServer, 8) {
		status, _ := attr(s.Attributes(), "http.response.status_code")
		typ, _ := attr(s.Attributes(), "error.type")
		odd = odd || typ == "vendor_specific_error"
		if want := status == "500" || typ == "overloaded_error"; (s.Status().Code == codes.Error) != want {
			t.Errorf("the span of a request answered %s with error %q is %v", status, typ, s.Status())
		}
	}
	if !odd {
		t.Error("no span names the upstream's error type as given")
	}
	rm := collect()

	fast := map[string]string{"modelgateway.alias": "fast", "modelgateway.deployment.id": d.ID, "modelgateway.api_key.id": "key_1"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range fast {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, c := range []struct {
		match map[string]string
		want  float64
	}{
		{with("modelgateway.endpoint", "messages", "http.response.status_code", "200"), 3},
		{with("modelgateway.endpoint", "count_tokens", "http.response.status_code", "200"), 1},
		{with("http.response.status_code", "400", "error.type", "invalid_request_error"), 1},
		{map[string]string{"modelgateway.alias": "*"}, 1},
		{map[string]string{"modelgateway.alias": "broken", "http.response.status_code": "500", "error.type": "api_error"}, 1},
		{with("http.response.status_code", "400", "error.type", "_OTHER"), 1},
		{map[string]string{"error.type": "vendor_specific_error"}, 0},
		{with("http.response.status_code", "200", "error.type", "overloaded_error"), 1},
		{map[string]string{}, 8},
	} {
		if got, _ := metricSum(rm, modelgateway.MetricRequests, c.match); got != c.want {
			t.Errorf("%s %v = %v, want %v", modelgateway.MetricRequests, c.match, got, c.want)
		}
	}
	if _, n := metricSum(rm, modelgateway.MetricRequestDuration, map[string]string{}); n != 8 {
		t.Errorf("%s holds %d readings, want 8", modelgateway.MetricRequestDuration, n)
	}
	if _, n := metricSum(rm, modelgateway.MetricTimeToFirstToken, with("modelgateway.endpoint", "messages")); n != 2 {
		t.Errorf("%s holds %d readings, want the two streams'", modelgateway.MetricTimeToFirstToken, n)
	}
	var want store.Tokens
	var cost float64
	for _, r := range rows {
		if r.Tokens != nil {
			want.Input, want.Output = want.Input+r.Tokens.Input, want.Output+r.Tokens.Output
			want.CacheWrite, want.CacheRead = want.CacheWrite+r.Tokens.CacheWrite, want.CacheRead+r.Tokens.CacheRead
		}
		if r.Cost != nil {
			cost += *r.Cost
		}
	}
	for typ, n := range map[string]int64{"input": want.Input, "output": want.Output, "cache_write": want.CacheWrite, "cache_read": want.CacheRead} {
		if got, _ := metricSum(rm, modelgateway.MetricTokens, map[string]string{"modelgateway.token.type": typ}); got != float64(n) {
			t.Errorf("%s %s = %v, want the ledger's %d", modelgateway.MetricTokens, typ, got, n)
		}
	}
	if want.Input == 0 || want.Output == 0 || want.CacheWrite == 0 || want.CacheRead == 0 {
		t.Fatalf("the ledger holds no tokens to compare: %+v", want)
	}
	if got, _ := metricSum(rm, modelgateway.MetricCost, map[string]string{}); math.Abs(got-cost) > 1e-12 || cost == 0 {
		t.Errorf("%s = %v, want the ledger's %v", modelgateway.MetricCost, got, cost)
	}
}
