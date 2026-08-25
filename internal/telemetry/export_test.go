package telemetry

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
)

// fakeCollector is an in-process OTLP/gRPC server. It proves the exporters
// actually put data on the wire in the expected shape, which an in-memory
// exporter cannot show.
type fakeCollector struct {
	coltrace.UnimplementedTraceServiceServer
	colmetrics.UnimplementedMetricsServiceServer
	collogs.UnimplementedLogsServiceServer

	mu       sync.Mutex
	spans    []*tracepb.Span
	metrics  []*metricspb.Metric
	logCount int
	headers  map[string][]string
}

func (c *fakeCollector) Export(ctx context.Context, req *coltrace.ExportTraceServiceRequest) (*coltrace.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			c.spans = append(c.spans, ss.Spans...)
		}
	}
	return &coltrace.ExportTraceServiceResponse{}, nil
}

func (c *fakeCollector) ExportMetrics(ctx context.Context, req *colmetrics.ExportMetricsServiceRequest) (*colmetrics.ExportMetricsServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			c.metrics = append(c.metrics, sm.Metrics...)
		}
	}
	return &colmetrics.ExportMetricsServiceResponse{}, nil
}

func (c *fakeCollector) ExportLogs(ctx context.Context, req *collogs.ExportLogsServiceRequest) (*collogs.ExportLogsServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			c.logCount += len(sl.LogRecords)
		}
	}
	return &collogs.ExportLogsServiceResponse{}, nil
}

func (c *fakeCollector) snapshot() ([]*tracepb.Span, []*metricspb.Metric, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*tracepb.Span(nil), c.spans...),
		append([]*metricspb.Metric(nil), c.metrics...),
		c.logCount
}

// metricsAdapter routes the metrics service to ExportMetrics, since all three
// services define a method named Export.
type metricsAdapter struct {
	colmetrics.UnimplementedMetricsServiceServer
	c *fakeCollector
}

func (a metricsAdapter) Export(ctx context.Context, req *colmetrics.ExportMetricsServiceRequest) (*colmetrics.ExportMetricsServiceResponse, error) {
	return a.c.ExportMetrics(ctx, req)
}

type logsAdapter struct {
	collogs.UnimplementedLogsServiceServer
	c *fakeCollector
}

func (a logsAdapter) Export(ctx context.Context, req *collogs.ExportLogsServiceRequest) (*collogs.ExportLogsServiceResponse, error) {
	return a.c.ExportLogs(ctx, req)
}

// startFakeCollector serves OTLP on a random port and returns its address.
func startFakeCollector(t *testing.T) (*fakeCollector, string) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	c := &fakeCollector{}
	srv := grpc.NewServer()
	coltrace.RegisterTraceServiceServer(srv, c)
	colmetrics.RegisterMetricsServiceServer(srv, metricsAdapter{c: c})
	collogs.RegisterLogsServiceServer(srv, logsAdapter{c: c})

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return c, lis.Addr().String()
}

// smokeTopology is the smallest topology that exercises all three signals.
func smokeTopology() config.Topology {
	return config.Topology{Services: []config.Service{{
		ServiceName: "svc",
		Instances:   []string{"svc-1"},
		Routes: []config.Route{{
			Route:    "GET /x",
			SpanKind: "server",
			Latency:  config.Latency{P50: 5, P99: 10, OutlierMultiplier: 1},
		}},
	}}}
}

// End-to-end: the exporters must reach a real OTLP endpoint, and Shutdown
// must flush what is still batched (spec C8).
func TestSetupAndShutdown_ExportsOverOTLP(t *testing.T) {
	collector, addr := startFakeCollector(t)

	// http:// selects an insecure connection to the local endpoint.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+addr)

	ctx := context.Background()
	topo := smokeTopology()

	providers, err := Setup(ctx, topo, Options{
		Backend:     BackendOTLP,
		Namespace:   "test",
		Environment: "test",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	em := providers.Emitter("svc")
	if em == nil {
		t.Fatal("no emitter for svc")
	}

	// Emit one span with a metric and a log inside it.
	spanCtx, span := em.Tracer().Start(ctx, "GET /x")
	em.RecordRED(spanCtx, "GET /x", 42, 200, false)
	em.Log(spanCtx, 9, "INFO", "hello")
	span.End()

	// Shutdown must flush; without it the batchers would still hold this data.
	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := providers.Shutdown(flushCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	spans, metrics, logs := collector.snapshot()

	if len(spans) != 1 {
		t.Errorf("collector received %d spans, want 1", len(spans))
	} else if spans[0].Name != "GET /x" {
		t.Errorf("span name = %q, want GET /x", spans[0].Name)
	}

	if len(metrics) == 0 {
		t.Error("collector received no metrics")
	}
	if logs == 0 {
		t.Error("collector received no log records")
	}
}

// Delta temporality is New Relic's requirement and the SDK's non-default, so
// assert it on the wire rather than trusting the selector in isolation.
func TestSetup_ExportsDeltaTemporality(t *testing.T) {
	collector, addr := startFakeCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+addr)

	ctx := context.Background()
	providers, err := Setup(ctx, smokeTopology(), Options{
		Backend:     BackendOTLP,
		Namespace:   "test",
		Environment: "test",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	em := providers.Emitter("svc")
	em.RecordRED(ctx, "GET /x", 10, 200, false)

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := providers.Shutdown(flushCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	_, metrics, _ := collector.snapshot()
	if len(metrics) == 0 {
		t.Fatal("no metrics received")
	}

	checked := 0
	for _, m := range metrics {
		switch data := m.Data.(type) {
		case *metricspb.Metric_Sum:
			checked++
			if data.Sum.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
				t.Errorf("counter %q temporality = %v, want DELTA", m.Name, data.Sum.AggregationTemporality)
			}
		case *metricspb.Metric_ExponentialHistogram:
			checked++
			if data.ExponentialHistogram.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
				t.Errorf("histogram %q temporality = %v, want DELTA", m.Name, data.ExponentialHistogram.AggregationTemporality)
			}
		case *metricspb.Metric_Histogram:
			t.Errorf("metric %q exported as an explicit-bucket histogram; exponential was requested", m.Name)
		}
	}
	if checked == 0 {
		t.Error("no sums or histograms found to verify temporality")
	}
}

// The duration histogram must be exported as an exponential histogram, which
// New Relic prefers for long-tailed distributions.
func TestSetup_ExportsExponentialHistogram(t *testing.T) {
	collector, addr := startFakeCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+addr)

	ctx := context.Background()
	providers, err := Setup(ctx, smokeTopology(), Options{Backend: BackendOTLP, Namespace: "t", Environment: "t"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	em := providers.Emitter("svc")
	for i := 0; i < 20; i++ {
		em.RecordRED(ctx, "GET /x", 10+i, 200, false)
	}

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := providers.Shutdown(flushCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	_, metrics, _ := collector.snapshot()
	found := false
	for _, m := range metrics {
		if m.Name != "http.server.request.duration" {
			continue
		}
		if _, ok := m.Data.(*metricspb.Metric_ExponentialHistogram); ok {
			found = true
		} else {
			t.Errorf("duration metric data type = %T, want ExponentialHistogram", m.Data)
		}
	}
	if !found {
		t.Error("http.server.request.duration was not exported")
	}
}

// Resource attributes identify the simulated service to the backend.
func TestSetup_SetsResourceAttributes(t *testing.T) {
	collector, addr := startFakeCollector(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+addr)

	ctx := context.Background()
	topo := config.Topology{Services: []config.Service{{
		ServiceName: "checkout",
		Tier:        "application",
		Instances:   []string{"c-1"},
		Routes: []config.Route{{
			Route:   "POST /order",
			Latency: config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1},
		}},
	}}}

	providers, err := Setup(ctx, topo, Options{
		Backend:     BackendOTLP,
		Namespace:   "ecom",
		Environment: "staging",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	_, span := providers.Emitter("checkout").Tracer().Start(ctx, "POST /order")
	span.End()

	flushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := providers.Shutdown(flushCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if len(collector.spans) == 0 {
		t.Fatal("no spans received")
	}
}
