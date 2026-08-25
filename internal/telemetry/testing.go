package telemetry

import (
	"context"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
)

// TestHarness exposes in-memory providers so tests can assert on emitted
// telemetry without a collector.
type TestHarness struct {
	Providers *Providers
	Spans     *tracetest.InMemoryExporter
	Reader    *sdkmetric.ManualReader
}

// NewTestHarness builds emitters for every service in topo, backed by
// in-memory exporters. Spans export synchronously so assertions need no
// flush; call CollectMetrics for metric data.
func NewTestHarness(t *testing.T, topo config.Topology) *TestHarness {
	t.Helper()

	spanExp := tracetest.NewInMemoryExporter()
	reader := sdkmetric.NewManualReader(
		sdkmetric.WithTemporalitySelector(deltaTemporalitySelector),
	)

	p := &Providers{emitters: make(map[string]*Emitter, len(topo.Services))}

	for _, svc := range topo.Services {
		res, err := buildResource(context.Background(), svc, Options{
			Namespace:   "test",
			Environment: "test",
		})
		if err != nil {
			t.Fatalf("building resource for %s: %v", svc.ServiceName, err)
		}

		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			// Synchronous export keeps assertions simple.
			sdktrace.WithSyncer(spanExp),
		)
		mp := sdkmetric.NewMeterProvider(
			sdkmetric.WithResource(res),
			sdkmetric.WithReader(reader),
		)
		lp := sdklog.NewLoggerProvider(sdklog.WithResource(res))

		em, err := newEmitter(
			svc.ServiceName,
			tp.Tracer(instrumentationScope),
			mp.Meter(instrumentationScope),
			lp.Logger(instrumentationScope),
		)
		if err != nil {
			t.Fatalf("building emitter for %s: %v", svc.ServiceName, err)
		}

		p.emitters[svc.ServiceName] = em
		p.traceProviders = append(p.traceProviders, tp)
		p.metricProviders = append(p.metricProviders, mp)
		p.logProviders = append(p.logProviders, lp)
	}

	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
	})

	return &TestHarness{Providers: p, Spans: spanExp, Reader: reader}
}

// CollectMetrics gathers the current metric data from the manual reader.
func (h *TestHarness) CollectMetrics(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.Reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collecting metrics: %v", err)
	}
	return rm
}

// CollectMetricNames returns the set of instrument names that have recorded
// data, for assertions that a metric was emitted at all.
func (h *TestHarness) CollectMetricNames(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, sm := range h.CollectMetrics(t).ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = true
		}
	}
	return out
}
