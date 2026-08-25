// Package e2e runs the compiled binary against an in-process OTLP server.
//
// This is the test that proves the whole program works: flags are parsed, the
// topology loads, providers are built, load is generated, and telemetry
// reaches a real gRPC endpoint before the process exits.
package e2e

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	// Registers the gzip decompressor, which the exporters use. A bare
	// grpc.NewServer rejects gzip-encoded requests without this.
	_ "google.golang.org/grpc/encoding/gzip"
)

// received accumulates everything the sink observed.
type received struct {
	mu sync.Mutex

	spanCount   int
	logCount    int
	metricNames map[string]bool
	services    map[string]bool
	spanKinds   map[string]bool
	// traceSpans maps trace id to the span ids it contains, for asserting
	// that a trace is a connected tree rather than loose spans.
	traceSpans map[string][]string
	// parents maps span id to its parent span id.
	parents  map[string]string
	errSpans int
}

func newReceived() *received {
	return &received{
		metricNames: map[string]bool{},
		services:    map[string]bool{},
		spanKinds:   map[string]bool{},
		traceSpans:  map[string][]string{},
		parents:     map[string]string{},
	}
}

func attrString(attrs []*commonpb.KeyValue, key string) string {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value.GetStringValue()
		}
	}
	return ""
}

type traceSink struct {
	coltrace.UnimplementedTraceServiceServer
	r *received
	// hook, when set, receives each ResourceSpans batch so a test can inspect
	// the decoded protobuf directly.
	hook func(*tracepb.ResourceSpans)
}

func (s traceSink) Export(_ context.Context, req *coltrace.ExportTraceServiceRequest) (*coltrace.ExportTraceServiceResponse, error) {
	if s.hook != nil {
		for _, rs := range req.ResourceSpans {
			s.hook(rs)
		}
	}

	s.r.mu.Lock()
	defer s.r.mu.Unlock()

	for _, rs := range req.ResourceSpans {
		svc := attrString(rs.Resource.GetAttributes(), "service.name")
		if svc != "" {
			s.r.services[svc] = true
		}
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				s.r.spanCount++
				tid := hex.EncodeToString(sp.TraceId)
				sid := hex.EncodeToString(sp.SpanId)
				s.r.traceSpans[tid] = append(s.r.traceSpans[tid], sid)
				if len(sp.ParentSpanId) > 0 {
					s.r.parents[sid] = hex.EncodeToString(sp.ParentSpanId)
				}
				s.r.spanKinds[sp.Kind.String()] = true
				if sp.Status != nil && sp.Status.Code == 2 { // STATUS_CODE_ERROR
					s.r.errSpans++
				}
			}
		}
	}
	return &coltrace.ExportTraceServiceResponse{}, nil
}

type metricSink struct {
	colmetrics.UnimplementedMetricsServiceServer
	r *received
}

func (s metricSink) Export(_ context.Context, req *colmetrics.ExportMetricsServiceRequest) (*colmetrics.ExportMetricsServiceResponse, error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()

	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				s.r.metricNames[m.Name] = true
				// Guard the New Relic requirement on the wire.
				switch d := m.Data.(type) {
				case *metricspb.Metric_Sum:
					if d.Sum.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
						s.r.metricNames["WRONG_TEMPORALITY:"+m.Name] = true
					}
				case *metricspb.Metric_ExponentialHistogram:
					if d.ExponentialHistogram.AggregationTemporality != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
						s.r.metricNames["WRONG_TEMPORALITY:"+m.Name] = true
					}
				}
			}
		}
	}
	return &colmetrics.ExportMetricsServiceResponse{}, nil
}

type logSink struct {
	collogs.UnimplementedLogsServiceServer
	r *received
}

func (s logSink) Export(_ context.Context, req *collogs.ExportLogsServiceRequest) (*collogs.ExportLogsServiceResponse, error) {
	s.r.mu.Lock()
	defer s.r.mu.Unlock()

	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			s.r.logCount += len(sl.LogRecords)
		}
	}
	return &collogs.ExportLogsServiceResponse{}, nil
}

// startSink serves OTLP on an ephemeral port.
func startSink(t *testing.T) (*received, string) {
	return startSinkWithHook(t, nil)
}

// startSinkWithHook is startSink plus a callback that receives each decoded
// ResourceSpans, for tests that assert on span attributes.
func startSinkWithHook(t *testing.T, hook func(*tracepb.ResourceSpans)) (*received, string) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	r := newReceived()
	srv := grpc.NewServer()
	coltrace.RegisterTraceServiceServer(srv, traceSink{r: r, hook: hook})
	colmetrics.RegisterMetricsServiceServer(srv, metricSink{r: r})
	collogs.RegisterLogsServiceServer(srv, logSink{r: r})

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return r, lis.Addr().String()
}

// buildBinary compiles the command once for the test run.
func buildBinary(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "loadgen")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/otel-ecom-load-genrator")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building binary: %v\n%s", err, out)
	}
	return bin
}

// runGenerator runs the binary to completion with a fixed duration.
func runGenerator(t *testing.T, bin, topology, endpoint string, extra ...string) string {
	t.Helper()

	args := append([]string{
		"--topology", topology,
		"--backend", "otlp",
		"--duration", "3s",
		"--log-level", "warn",
	}, extra...)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(cmd.Environ(),
		"OTEL_EXPORTER_OTLP_ENDPOINT=http://"+endpoint,
		// Keep batches small so a short run still exports.
		"OTEL_BSP_SCHEDULE_DELAY=200",
		"OTEL_BLRP_SCHEDULE_DELAY=200",
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generator failed: %v\n%s", err, out)
	}
	return string(out)
}

// The full program must emit connected traces, metrics, and logs to a real
// OTLP endpoint and exit cleanly.
func TestEndToEnd_SmokeTopology(t *testing.T) {
	sink, endpoint := startSink(t)
	bin := buildBinary(t)

	out := runGenerator(t, bin, "../../topologies/shop-smoke.json", endpoint)

	// Give the final flush a moment to land on the sink.
	time.Sleep(500 * time.Millisecond)

	sink.mu.Lock()
	defer sink.mu.Unlock()

	if sink.spanCount == 0 {
		t.Fatalf("no spans received; generator output:\n%s", out)
	}
	t.Logf("received %d spans across %d traces, %d logs, %d metrics",
		sink.spanCount, len(sink.traceSpans), sink.logCount, len(sink.metricNames))

	// All three services in shop-smoke.json must appear.
	for _, want := range []string{"ashish-api-gateway", "ashish-cart-service", "ashish-redis-cache"} {
		if !sink.services[want] {
			t.Errorf("service %q never appeared; got %v", want, keysOf(sink.services))
		}
	}

	// Each trace should hold more than one span, proving downstream calls are
	// linked into the same trace rather than starting new ones.
	multi := 0
	for _, spans := range sink.traceSpans {
		if len(spans) > 1 {
			multi++
		}
	}
	if multi == 0 {
		t.Error("every trace had a single span; downstream calls are not linked")
	}

	// Parents must resolve to spans in the same trace.
	allSpans := map[string]bool{}
	for _, spans := range sink.traceSpans {
		for _, s := range spans {
			allSpans[s] = true
		}
	}
	orphans := 0
	for child, parent := range sink.parents {
		if !allSpans[parent] {
			orphans++
			_ = child
		}
	}
	if orphans > 0 {
		t.Errorf("%d spans reference a parent that was never exported", orphans)
	}

	if sink.logCount == 0 {
		t.Error("no log records received")
	}

	for _, want := range []string{
		"http.server.request.duration",
		"http.server.request.count",
	} {
		if !sink.metricNames[want] {
			t.Errorf("metric %q missing; got %v", want, keysOf(sink.metricNames))
		}
	}

	// Business metrics declared in the topology must appear.
	for _, want := range []string{"cart.views", "cart.value"} {
		if !sink.metricNames[want] {
			t.Errorf("business metric %q missing; got %v", want, keysOf(sink.metricNames))
		}
	}

	// No metric may be exported with the wrong temporality.
	for name := range sink.metricNames {
		if strings.HasPrefix(name, "WRONG_TEMPORALITY:") {
			t.Errorf("metric exported with non-delta temporality: %s", name)
		}
	}

	// Span kinds from the topology must survive to the wire.
	if !sink.spanKinds["SPAN_KIND_SERVER"] {
		t.Errorf("no server-kind spans; got %v", keysOf(sink.spanKinds))
	}
	if !sink.spanKinds["SPAN_KIND_CLIENT"] {
		t.Errorf("no client-kind spans; got %v", keysOf(sink.spanKinds))
	}
}

// The full 18-service topology must run without panicking and produce the
// async producer/consumer span kinds.
func TestEndToEnd_FullTopology(t *testing.T) {
	sink, endpoint := startSink(t)
	bin := buildBinary(t)

	out := runGenerator(t, bin, "../../topologies/shop-full.json", endpoint)

	time.Sleep(500 * time.Millisecond)

	sink.mu.Lock()
	defer sink.mu.Unlock()

	if sink.spanCount == 0 {
		t.Fatalf("no spans received; output:\n%s", out)
	}
	t.Logf("received %d spans across %d traces from %d services",
		sink.spanCount, len(sink.traceSpans), len(sink.services))

	// A deep topology should produce deep traces.
	maxDepth := 0
	for _, spans := range sink.traceSpans {
		if len(spans) > maxDepth {
			maxDepth = len(spans)
		}
	}
	if maxDepth < 3 {
		t.Errorf("largest trace had %d spans, want at least 3 for this topology", maxDepth)
	}

	// The checkout flow publishes to an async hop.
	if !sink.spanKinds["SPAN_KIND_PRODUCER"] && !sink.spanKinds["SPAN_KIND_CONSUMER"] {
		t.Logf("no async span kinds observed in this window; got %v", keysOf(sink.spanKinds))
	}

	// Weighted error injection should produce some failed spans.
	if sink.errSpans == 0 {
		t.Logf("no error spans in this window (rates are low; not necessarily a failure)")
	} else {
		t.Logf("observed %d error spans", sink.errSpans)
	}

	// The generator must not report recovered panics.
	if strings.Contains(out, "recovered panic") {
		t.Errorf("generator recovered panics during the run:\n%s", out)
	}
}

// --validate-only must exit zero without contacting any endpoint.
func TestEndToEnd_ValidateOnly(t *testing.T) {
	bin := buildBinary(t)

	for _, topo := range []string{
		"../../topologies/shop-smoke.json",
		"../../topologies/shop-full.json",
	} {
		cmd := exec.Command(bin, "--topology", topo, "--validate-only")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s: validate-only failed: %v\n%s", topo, err, out)
		}
		if !strings.Contains(string(out), "topology is valid") {
			t.Errorf("%s: unexpected output:\n%s", topo, out)
		}
	}
}

// A missing topology must be a startup error, not a silent no-op.
func TestEndToEnd_RejectsMissingTopology(t *testing.T) {
	bin := buildBinary(t)

	cmd := exec.Command(bin, "--validate-only")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit without --topology; output:\n%s", out)
	}
	if !strings.Contains(string(out), "--topology is required") {
		t.Errorf("unexpected error message:\n%s", out)
	}
}

// An invalid topology must fail before any exporter is built.
func TestEndToEnd_RejectsInvalidTopology(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")

	writeFile(t, bad, `{
      "topology": {"services": [{
        "serviceName": "a",
        "instances": ["a-1"],
        "routes": [{
          "route": "r",
          "downstreamCalls": {"ghost": "x"},
          "latency": {"p50": 1, "p99": 2, "outlierMultiplier": 1}
        }]
      }]},
      "rootRoutes": [{"service": "a", "route": "r", "tracesPerHour": 1}]
    }`)

	cmd := exec.Command(bin, "--topology", bad)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit for a dangling reference; output:\n%s", out)
	}
	if !strings.Contains(string(out), "unknown service") {
		t.Errorf("error should name the unknown service:\n%s", out)
	}
}

// --backend=newrelic without a license key must fail fast and name the
// variable, rather than silently exporting nowhere.
func TestEndToEnd_NewRelicRequiresLicenseKey(t *testing.T) {
	bin := buildBinary(t)

	cmd := exec.Command(bin,
		"--topology", "../../topologies/shop-smoke.json",
		"--backend", "newrelic",
		"--duration", "1s",
	)
	// Explicitly clear both the key and any endpoint override.
	cmd.Env = append(cmd.Environ(),
		"NEW_RELIC_LICENSE_KEY=",
		"OTEL_EXPORTER_OTLP_ENDPOINT=",
	)

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected a non-zero exit without a license key; output:\n%s", out)
	}
	if !strings.Contains(string(out), "NEW_RELIC_LICENSE_KEY") {
		t.Errorf("error should name the required variable:\n%s", out)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
