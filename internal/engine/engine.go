// Package engine walks the topology to emit telemetry and drives the
// per-root-route generation rates.
package engine

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/chaos"
	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/telemetry"
)

// TimingMode selects how span end times are computed.
type TimingMode string

const (
	// TimingSynthetic computes an end time without waiting. Throughput is
	// high, but because children are emitted concurrently with independently
	// sampled durations, a parent can end before its children.
	TimingSynthetic TimingMode = "synthetic"
	// TimingNested sleeps so that children genuinely complete within their
	// parent's window, making waterfall views and critical-path analysis
	// correct at the cost of throughput.
	TimingNested TimingMode = "nested"
)

// ParseTimingMode converts a flag value to a TimingMode.
func ParseTimingMode(s string) (TimingMode, error) {
	switch TimingMode(strings.ToLower(strings.TrimSpace(s))) {
	case TimingSynthetic:
		return TimingSynthetic, nil
	case TimingNested:
		return TimingNested, nil
	default:
		return "", &badTimingError{value: s}
	}
}

type badTimingError struct{ value string }

func (e *badTimingError) Error() string {
	return "unknown timing mode " + e.value + " (want synthetic or nested)"
}

// Options configures the engine.
type Options struct {
	Timing TimingMode
	Logger *slog.Logger
}

// Engine emits telemetry by walking a validated topology.
type Engine struct {
	topo      config.Topology
	providers *telemetry.Providers
	timing    TimingMode
	log       *slog.Logger

	// Run counters, reported periodically so a long run is observable from
	// its own logs rather than only from the backend.
	spans     atomic64
	traces    atomic64
	errSpans  atomic64
	logsCount atomic64

	// panics counts recovered emission panics, so one bad route degrades a
	// single trace rather than terminating the process (spec C12).
	panics atomic64
}

// Stats is a snapshot of what the engine has emitted.
type Stats struct {
	Traces     int64
	Spans      int64
	ErrorSpans int64
	Logs       int64
	Panics     int64
}

// Stats returns the current counters.
func (e *Engine) Stats() Stats {
	return Stats{
		Traces:     e.traces.load(),
		Spans:      e.spans.load(),
		ErrorSpans: e.errSpans.load(),
		Logs:       e.logsCount.load(),
		Panics:     e.panics.load(),
	}
}

// atomic64 is a minimal atomic counter, kept local to avoid exposing the
// mutex in the Engine's API surface.
type atomic64 struct {
	mu sync.Mutex
	n  int64
}

func (a *atomic64) inc() {
	a.mu.Lock()
	a.n++
	a.mu.Unlock()
}

func (a *atomic64) load() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

// New builds an Engine. The topology must already have been validated.
func New(topo config.Topology, providers *telemetry.Providers, opts Options) *Engine {
	timing := opts.Timing
	if timing == "" {
		timing = TimingSynthetic
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{topo: topo, providers: providers, timing: timing, log: logger}
}

// Panics reports how many emission panics have been recovered.
func (e *Engine) Panics() int64 { return e.panics.load() }

// EmitTrace emits one span for the given service and route, then recurses
// into that route's downstream calls.
//
// An unknown service or route is a no-op: validation is what guarantees
// references resolve, so reaching here with a bad reference means the caller
// skipped validation.
func (e *Engine) EmitTrace(ctx context.Context, serviceName, routeName string) {
	svc := e.topo.FindService(serviceName)
	if svc == nil {
		return
	}
	route := svc.FindRoute(routeName)
	if route == nil {
		return
	}
	em := e.providers.Emitter(serviceName)
	if em == nil {
		return
	}

	defer e.recoverEmission(serviceName, routeName)

	start := time.Now()
	// A span with no valid parent starts a new trace.
	if !trace.SpanContextFromContext(ctx).IsValid() {
		e.traces.inc()
	}
	e.spans.inc()

	childCtx, span := em.Tracer().Start(ctx, routeName,
		trace.WithSpanKind(spanKind(route.SpanKind)),
		trace.WithTimestamp(start),
	)

	span.SetAttributes(attribute.String("service.instance.id", chaos.PickInstance(svc.Instances)))

	// Semantic-convention attributes derived from the route.
	//
	// http.route in particular is what backends use to name a transaction from
	// a server span. Without it New Relic shows the transaction as "unknown",
	// because a span name alone is not a transaction name.
	span.SetAttributes(routeAttributes(routeName, route.SpanKind)...)

	// Service-level attributes first, then route-level, so a route can
	// override a service default.
	svcSet := chaos.PickAttributeSet(svc.AttributeSets)
	routeSet := chaos.PickAttributeSet(route.AttributeSets)
	applyAttributes(span, svcSet)
	applyAttributes(span, routeSet)

	addEvents(span, chaos.PickEventSet(svc.EventSets), start)
	addEvents(span, chaos.PickEventSet(route.EventSets), start)

	failed, statusCode := resolveOutcome(svcSet, routeSet)
	if failed {
		errType := errorType(statusCode)
		span.SetAttributes(attribute.String("error.type", errType))
		span.SetStatus(codes.Error, errType)
		em.Log(childCtx, otellog.SeverityError, "ERROR",
			serviceName+" "+routeName+" failed",
			attribute.String("error.type", errType),
			attribute.String("http.route", routeName),
		)
		e.errSpans.inc()
	} else {
		span.SetStatus(codes.Ok, "")
		em.Log(childCtx, otellog.SeverityInfo, "INFO",
			serviceName+" "+routeName+" completed",
			attribute.String("http.route", routeName),
		)
	}
	e.logsCount.inc()

	durationMillis := chaos.SampleLatencyMillis(route.Latency)

	// Recurse before ending the span so children are nested inside it.
	e.emitDownstreams(childCtx, route)

	if e.timing == TimingNested {
		// Sleep the remainder so the reported duration matches wall clock.
		if remaining := time.Duration(durationMillis)*time.Millisecond - time.Since(start); remaining > 0 {
			time.Sleep(remaining)
		}
		span.End()
	} else {
		span.End(trace.WithTimestamp(start.Add(time.Duration(durationMillis) * time.Millisecond)))
	}

	em.RecordRED(childCtx, routeName, durationMillis, statusCode, failed)
	e.recordBusinessMetrics(childCtx, em, route)
}

// emitDownstreams fans out to every downstream call and waits for them, so
// the parent's span is still open while children are created.
func (e *Engine) emitDownstreams(ctx context.Context, route *config.Route) {
	if len(route.DownstreamCalls) == 0 {
		return
	}

	var wg sync.WaitGroup
	for dService, dRoute := range route.DownstreamCalls {
		wg.Add(1)
		go func(svc, rt string) {
			defer wg.Done()
			e.EmitTrace(ctx, svc, rt)
		}(dService, dRoute)
	}
	wg.Wait()
}

// recoverEmission keeps one malformed route from taking down the process.
func (e *Engine) recoverEmission(service, route string) {
	if r := recover(); r != nil {
		e.panics.inc()
		e.log.Error("recovered panic while emitting",
			"service", service, "route", route, "panic", r)
	}
}

// recordBusinessMetrics records the instruments declared on a route.
func (e *Engine) recordBusinessMetrics(ctx context.Context, em *telemetry.Emitter, route *config.Route) {
	for _, bm := range route.Metrics.Business {
		switch bm.Kind {
		case "counter":
			c, err := em.Counter(bm.Name, bm.Unit)
			if err != nil {
				e.log.Warn("creating counter failed", "name", bm.Name, "error", err)
				continue
			}
			c.Add(ctx, 1)
		case "histogram":
			h, err := em.Histogram(bm.Name, bm.Unit)
			if err != nil {
				e.log.Warn("creating histogram failed", "name", bm.Name, "error", err)
				continue
			}
			h.Record(ctx, sampleInRange(bm.Min, bm.Max))
		}
	}
}

// sampleInRange returns a value in [min, max], defaulting to 1 when the
// range is unset.
func sampleInRange(min, max float64) float64 {
	if max <= min {
		if min > 0 {
			return min
		}
		return 1
	}
	return min + chaos.Float64()*(max-min)
}

// applyAttributes sets a selected attribute set's attributes on the span.
func applyAttributes(span trace.Span, set *config.AttributeSet) {
	if set == nil {
		return
	}
	if kvs := telemetry.ConvertAttributes(set.Attributes); len(kvs) > 0 {
		span.SetAttributes(kvs...)
	}
}

// addEvents adds a selected event set's events to the span.
func addEvents(span trace.Span, set *config.EventSet, start time.Time) {
	if set == nil {
		return
	}
	for _, ev := range set.Events {
		span.AddEvent(ev.Name,
			trace.WithAttributes(telemetry.ConvertAttributes(ev.Attributes)...),
			trace.WithTimestamp(start),
		)
	}
}

// resolveOutcome reports whether the span failed and its status code, taking
// the route-level set as authoritative over the service-level one.
func resolveOutcome(svcSet, routeSet *config.AttributeSet) (failed bool, statusCode int) {
	for _, set := range []*config.AttributeSet{svcSet, routeSet} {
		if set == nil {
			continue
		}
		if set.HasErrorDirective() {
			failed = true
		}
		if code, ok := statusCodeOf(set); ok {
			statusCode = code
		}
	}
	return failed, statusCode
}

// statusCodeOf extracts an http.response.status_code attribute, which JSON
// decoding delivers as a float64.
func statusCodeOf(set *config.AttributeSet) (int, bool) {
	v, ok := set.Attributes["http.response.status_code"]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

// errorType names the failure for the error.type attribute, using the status
// code when one is configured.
func errorType(statusCode int) string {
	switch {
	case statusCode >= 500:
		return "upstream_unavailable"
	case statusCode >= 400:
		return "client_error"
	default:
		return "internal_error"
	}
}

// spanKind maps a configured span kind to the OTel enum. An empty or unknown
// value becomes internal; validation rejects unknown values before this runs.
func spanKind(s string) trace.SpanKind {
	switch strings.ToLower(s) {
	case "server":
		return trace.SpanKindServer
	case "client":
		return trace.SpanKindClient
	case "producer":
		return trace.SpanKindProducer
	case "consumer":
		return trace.SpanKindConsumer
	default:
		return trace.SpanKindInternal
	}
}
