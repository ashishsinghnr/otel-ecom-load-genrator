package engine

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/telemetry"
)

// threeTierTopology is web -> cart -> redis, a linear chain whose span count
// and parentage are easy to assert.
func threeTierTopology() config.Topology {
	return config.Topology{Services: []config.Service{
		{
			ServiceName: "web",
			Instances:   []string{"web-1"},
			Routes: []config.Route{{
				Route:           "GET /cart",
				SpanKind:        "server",
				DownstreamCalls: map[string]string{"cart": "GET /items"},
				Latency:         config.Latency{P50: 10, P99: 20, OutlierMultiplier: 1},
			}},
		},
		{
			ServiceName: "cart",
			Instances:   []string{"cart-1", "cart-2"},
			Routes: []config.Route{{
				Route:           "GET /items",
				SpanKind:        "server",
				DownstreamCalls: map[string]string{"redis": "GET"},
				Latency:         config.Latency{P50: 5, P99: 10, OutlierMultiplier: 1},
			}},
		},
		{
			ServiceName: "redis",
			Instances:   []string{"redis-1"},
			Routes: []config.Route{{
				Route:    "GET",
				SpanKind: "client",
				Latency:  config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1},
			}},
		},
	}}
}

func spanByName(spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

func stubsByName(t *testing.T, h *telemetry.TestHarness, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	snap := h.Spans.GetSpans().Snapshots()
	s := spanByName(snap, name)
	if s == nil {
		t.Fatalf("span %q not found; got %d spans", name, len(snap))
	}
	return s
}

func TestEmitTrace_EmitsOneSpanPerRoute(t *testing.T) {
	topo := threeTierTopology()
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "web", "GET /cart")

	spans := h.Spans.GetSpans().Snapshots()
	if len(spans) != 3 {
		var names []string
		for _, s := range spans {
			names = append(names, s.Name())
		}
		t.Fatalf("got %d spans %v, want 3", len(spans), names)
	}

	for _, want := range []string{"GET /cart", "GET /items", "GET"} {
		if spanByName(spans, want) == nil {
			t.Errorf("missing span %q", want)
		}
	}
}

// Parent/child linkage is what makes a trace a trace rather than three
// unrelated spans.
func TestEmitTrace_LinksParentsAndChildren(t *testing.T) {
	topo := threeTierTopology()
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "web", "GET /cart")

	root := stubsByName(t, h, "GET /cart")
	mid := stubsByName(t, h, "GET /items")
	leaf := stubsByName(t, h, "GET")

	// All three share one trace id.
	tid := root.SpanContext().TraceID()
	if mid.SpanContext().TraceID() != tid || leaf.SpanContext().TraceID() != tid {
		t.Error("spans do not share a trace id")
	}

	// The root has no parent; each child points at its parent.
	if root.Parent().IsValid() {
		t.Error("root span should have no valid parent")
	}
	if mid.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Error("middle span's parent is not the root")
	}
	if leaf.Parent().SpanID() != mid.SpanContext().SpanID() {
		t.Error("leaf span's parent is not the middle span")
	}
}

func TestEmitTrace_AppliesSpanKind(t *testing.T) {
	topo := threeTierTopology()
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "web", "GET /cart")

	if got := stubsByName(t, h, "GET /cart").SpanKind(); got != trace.SpanKindServer {
		t.Errorf("root kind = %v, want server", got)
	}
	if got := stubsByName(t, h, "GET").SpanKind(); got != trace.SpanKindClient {
		t.Errorf("leaf kind = %v, want client", got)
	}
}

// An empty spanKind must default to internal rather than unspecified.
func TestEmitTrace_DefaultsSpanKindToInternal(t *testing.T) {
	topo := config.Topology{Services: []config.Service{{
		ServiceName: "svc",
		Instances:   []string{"svc-1"},
		Routes: []config.Route{{
			Route:   "work",
			Latency: config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1},
		}},
	}}}
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "svc", "work")

	if got := stubsByName(t, h, "work").SpanKind(); got != trace.SpanKindInternal {
		t.Errorf("kind = %v, want internal", got)
	}
}

func TestEmitTrace_SetsInstanceID(t *testing.T) {
	topo := threeTierTopology()
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "web", "GET /cart")

	mid := stubsByName(t, h, "GET /items")
	var found string
	for _, kv := range mid.Attributes() {
		if kv.Key == "service.instance.id" {
			found = kv.Value.AsString()
		}
	}
	if found != "cart-1" && found != "cart-2" {
		t.Errorf("service.instance.id = %q, want one of the declared cart instances", found)
	}
}

// C10: an error directive must produce a real error status, not just an
// attribute, or the backend will not count it as a failure.
func TestEmitTrace_ErrorDirectiveSetsSpanStatus(t *testing.T) {
	topo := config.Topology{Services: []config.Service{{
		ServiceName: "svc",
		Instances:   []string{"svc-1"},
		Routes: []config.Route{{
			Route:    "fail",
			SpanKind: "server",
			Latency:  config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1},
			AttributeSets: []config.AttributeSet{{
				Weight: 1,
				Attributes: map[string]interface{}{
					"error":                     true,
					"http.response.status_code": float64(503),
				},
			}},
		}},
	}}}
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "svc", "fail")

	s := stubsByName(t, h, "fail")
	if s.Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", s.Status().Code)
	}
	if s.Status().Description == "" {
		t.Error("error status should carry a description")
	}

	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	if _, ok := attrs["error"]; ok {
		t.Error("the error directive must not be exported as an attribute")
	}
	if v, ok := attrs["error.type"]; !ok || v.AsString() == "" {
		t.Error("expected an error.type attribute")
	}
	if v, ok := attrs["http.response.status_code"]; !ok || v.AsInt64() != 503 {
		t.Errorf("status code attribute = %v, want 503", v)
	}
}

func TestEmitTrace_SuccessSetsOKStatus(t *testing.T) {
	topo := threeTierTopology()
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "web", "GET /cart")

	if got := stubsByName(t, h, "GET /cart").Status().Code; got == codes.Error {
		t.Errorf("status = %v, want non-error", got)
	}
}

func TestEmitTrace_AddsEvents(t *testing.T) {
	topo := config.Topology{Services: []config.Service{{
		ServiceName: "pay",
		Instances:   []string{"pay-1"},
		Routes: []config.Route{{
			Route:   "authorize",
			Latency: config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1},
			EventSets: []config.EventSet{{
				Weight: 1,
				Events: []config.Event{
					{Name: "payment.authorized", Attributes: map[string]interface{}{"amount": float64(42)}},
					{Name: "receipt.queued"},
				},
			}},
		}},
	}}}
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "pay", "authorize")

	events := stubsByName(t, h, "authorize").Events()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Name != "payment.authorized" {
		t.Errorf("first event = %q", events[0].Name)
	}
	// The event's integral attribute must survive as an int.
	var amount int64 = -1
	for _, kv := range events[0].Attributes {
		if kv.Key == "amount" {
			amount = kv.Value.AsInt64()
		}
	}
	if amount != 42 {
		t.Errorf("event amount = %d, want 42", amount)
	}
}

// Service-level and route-level attribute sets must both apply.
func TestEmitTrace_MergesServiceAndRouteAttributes(t *testing.T) {
	topo := config.Topology{Services: []config.Service{{
		ServiceName:   "svc",
		Instances:     []string{"svc-1"},
		AttributeSets: []config.AttributeSet{{Weight: 1, Attributes: map[string]interface{}{"version": "v9"}}},
		Routes: []config.Route{{
			Route:         "work",
			Latency:       config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1},
			AttributeSets: []config.AttributeSet{{Weight: 1, Attributes: map[string]interface{}{"tenant": "acme"}}},
		}},
	}}}
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "svc", "work")

	got := map[attribute.Key]string{}
	for _, kv := range stubsByName(t, h, "work").Attributes() {
		got[kv.Key] = kv.Value.Emit()
	}
	if got["version"] != "v9" {
		t.Errorf("service attribute missing: %v", got)
	}
	if got["tenant"] != "acme" {
		t.Errorf("route attribute missing: %v", got)
	}
}

// Fan-out: one route calling two downstreams produces both children.
func TestEmitTrace_FansOutToAllDownstreams(t *testing.T) {
	topo := config.Topology{Services: []config.Service{
		{
			ServiceName: "checkout",
			Instances:   []string{"c-1"},
			Routes: []config.Route{{
				Route:    "POST /checkout",
				SpanKind: "server",
				DownstreamCalls: map[string]string{
					"payment":   "POST /authorize",
					"inventory": "POST /reserve",
				},
				Latency: config.Latency{P50: 10, P99: 20, OutlierMultiplier: 1},
			}},
		},
		{
			ServiceName: "payment", Instances: []string{"p-1"},
			Routes: []config.Route{{Route: "POST /authorize", Latency: config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1}}},
		},
		{
			ServiceName: "inventory", Instances: []string{"i-1"},
			Routes: []config.Route{{Route: "POST /reserve", Latency: config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1}}},
		},
	}}
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "checkout", "POST /checkout")

	spans := h.Spans.GetSpans().Snapshots()
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(spans))
	}
	root := stubsByName(t, h, "POST /checkout")
	for _, child := range []string{"POST /authorize", "POST /reserve"} {
		s := stubsByName(t, h, child)
		if s.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%q is not parented to the root", child)
		}
	}
}

// An unknown service or route must be a no-op rather than a panic, since
// validation is what guarantees they resolve.
func TestEmitTrace_UnknownTargetsAreNoOps(t *testing.T) {
	topo := threeTierTopology()
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "ghost", "GET /cart")
	e.EmitTrace(context.Background(), "web", "GET /nope")

	if n := len(h.Spans.GetSpans().Snapshots()); n != 0 {
		t.Errorf("emitted %d spans for unknown targets, want 0", n)
	}
}

// Spans must have a positive duration; a zero-length span is useless in a
// waterfall view.
func TestEmitTrace_SpansHavePositiveDuration(t *testing.T) {
	topo := threeTierTopology()
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "web", "GET /cart")

	for _, s := range h.Spans.GetSpans().Snapshots() {
		d := s.EndTime().Sub(s.StartTime())
		if d <= 0 {
			t.Errorf("span %q has non-positive duration %v", s.Name(), d)
		}
	}
}

// RED metrics must be recorded for every visited route.
func TestEmitTrace_RecordsREDMetrics(t *testing.T) {
	topo := threeTierTopology()
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "web", "GET /cart")

	names := h.CollectMetricNames(t)
	for _, want := range []string{"http.server.request.duration", "http.server.request.count"} {
		if !names[want] {
			t.Errorf("metric %q not recorded; got %v", want, keys(names))
		}
	}
}

func TestEmitTrace_RecordsBusinessMetrics(t *testing.T) {
	topo := config.Topology{Services: []config.Service{{
		ServiceName: "checkout",
		Instances:   []string{"c-1"},
		Routes: []config.Route{{
			Route:   "POST /checkout",
			Latency: config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1},
			Metrics: config.RouteMetrics{Business: []config.BusinessMetric{
				{Name: "orders.placed", Kind: "counter", Unit: "{order}"},
				{Name: "cart.value", Kind: "histogram", Unit: "USD", Min: 10, Max: 100},
			}},
		}},
	}}}
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "checkout", "POST /checkout")

	names := h.CollectMetricNames(t)
	for _, want := range []string{"orders.placed", "cart.value"} {
		if !names[want] {
			t.Errorf("business metric %q not recorded; got %v", want, keys(names))
		}
	}
}

// Error routes must increment the error counter.
func TestEmitTrace_RecordsErrorMetric(t *testing.T) {
	topo := config.Topology{Services: []config.Service{{
		ServiceName: "svc",
		Instances:   []string{"svc-1"},
		Routes: []config.Route{{
			Route:   "fail",
			Latency: config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1},
			AttributeSets: []config.AttributeSet{{
				Weight:     1,
				Attributes: map[string]interface{}{"error": true},
			}},
		}},
	}}}
	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})

	e.EmitTrace(context.Background(), "svc", "fail")

	if !h.CollectMetricNames(t)["http.server.error.count"] {
		t.Error("error counter not recorded for a failing route")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
