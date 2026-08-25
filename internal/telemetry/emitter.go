package telemetry

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Emitter groups one simulated service's signals behind a single resource, so
// its traces, metrics, and logs all agree on which service produced them.
type Emitter struct {
	ServiceName string

	tracer trace.Tracer
	meter  metric.Meter
	logger otellog.Logger

	// RED instruments, created once per service.
	reqDuration metric.Float64Histogram
	reqCount    metric.Int64Counter
	errCount    metric.Int64Counter

	// Business instruments, created lazily by name since they are declared
	// per route in the topology.
	mu         sync.Mutex
	counters   map[string]metric.Int64Counter
	histograms map[string]metric.Float64Histogram
}

// Tracer exposes the service's tracer.
func (e *Emitter) Tracer() trace.Tracer { return e.tracer }

// newEmitter builds an Emitter and its RED instruments.
func newEmitter(serviceName string, tracer trace.Tracer, meter metric.Meter, logger otellog.Logger) (*Emitter, error) {
	e := &Emitter{
		ServiceName: serviceName,
		tracer:      tracer,
		meter:       meter,
		logger:      logger,
		counters:    map[string]metric.Int64Counter{},
		histograms:  map[string]metric.Float64Histogram{},
	}

	var err error
	// Duration is in seconds, per OTel semantic conventions.
	e.reqDuration, err = meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithDescription("Duration of inbound requests"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating request duration histogram: %w", err)
	}

	e.reqCount, err = meter.Int64Counter(
		"http.server.request.count",
		metric.WithDescription("Count of inbound requests"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating request counter: %w", err)
	}

	e.errCount, err = meter.Int64Counter(
		"http.server.error.count",
		metric.WithDescription("Count of failed inbound requests"),
		metric.WithUnit("{error}"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating error counter: %w", err)
	}

	return e, nil
}

// RecordRED records the technical metrics for one route visit. durationMillis
// is converted to seconds to match the semantic convention unit.
func (e *Emitter) RecordRED(ctx context.Context, route string, durationMillis int, statusCode int, failed bool) {
	attrs := []attribute.KeyValue{
		semconv.HTTPRouteKey.String(route),
		attribute.String("service.name", e.ServiceName),
	}
	if statusCode > 0 {
		attrs = append(attrs, semconv.HTTPResponseStatusCode(statusCode))
	}

	set := metric.WithAttributes(attrs...)
	e.reqDuration.Record(ctx, float64(durationMillis)/1000.0, set)
	e.reqCount.Add(ctx, 1, set)
	if failed {
		e.errCount.Add(ctx, 1, set)
	}
}

// Counter returns a lazily-created named counter.
func (e *Emitter) Counter(name, unit string) (metric.Int64Counter, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if c, ok := e.counters[name]; ok {
		return c, nil
	}
	opts := []metric.Int64CounterOption{metric.WithDescription("Business counter " + name)}
	if unit != "" {
		opts = append(opts, metric.WithUnit(unit))
	}
	c, err := e.meter.Int64Counter(name, opts...)
	if err != nil {
		return nil, err
	}
	e.counters[name] = c
	return c, nil
}

// Histogram returns a lazily-created named histogram.
func (e *Emitter) Histogram(name, unit string) (metric.Float64Histogram, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if h, ok := e.histograms[name]; ok {
		return h, nil
	}
	opts := []metric.Float64HistogramOption{metric.WithDescription("Business histogram " + name)}
	if unit != "" {
		opts = append(opts, metric.WithUnit(unit))
	}
	h, err := e.meter.Float64Histogram(name, opts...)
	if err != nil {
		return nil, err
	}
	e.histograms[name] = h
	return h, nil
}

// Log emits a log record inside ctx, so the active span's trace_id and
// span_id are attached and the backend can pivot from trace to logs.
func (e *Emitter) Log(ctx context.Context, severity otellog.Severity, severityText, body string, attrs ...attribute.KeyValue) {
	var rec otellog.Record
	rec.SetSeverity(severity)
	rec.SetSeverityText(severityText)
	rec.SetBody(attribute.StringValue(body))
	if len(attrs) > 0 {
		rec.AddAttributes(attrs...)
	}
	e.logger.Emit(ctx, rec)
}
