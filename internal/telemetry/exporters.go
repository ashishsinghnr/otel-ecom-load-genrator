package telemetry

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc"
)

// exporterFactory builds one service's three exporters for the configured
// protocol. It exists so the grpc/http branch lives in one place rather than
// being repeated for each signal at each call site.
type exporterFactory struct {
	protocol Protocol
	headers  map[string]string

	// endpoint is the resolved URL, empty to let the SDK read
	// OTEL_EXPORTER_OTLP_ENDPOINT itself.
	endpoint string
	// insecure forces plaintext, used by the local backend.
	insecure bool
	// conn is the shared gRPC connection; nil for HTTP.
	conn *grpc.ClientConn
}

// hostPort splits the resolved endpoint into a "host:port" for the HTTP
// exporters' WithEndpoint, and reports whether the connection is plaintext.
//
// ok is false when no endpoint was resolved, which means the SDK should read
// OTEL_EXPORTER_OTLP_ENDPOINT itself. That variable carries a full URL and the
// SDK appends the signal path to it correctly.
func (f exporterFactory) hostPort() (host string, insecure bool, ok bool) {
	if f.endpoint == "" {
		return "", false, false
	}

	insecure = f.insecure
	trimmed := f.endpoint
	switch {
	case strings.HasPrefix(trimmed, "http://"):
		trimmed = strings.TrimPrefix(trimmed, "http://")
		insecure = true
	case strings.HasPrefix(trimmed, "https://"):
		trimmed = strings.TrimPrefix(trimmed, "https://")
	}

	// Drop any path; WithEndpoint wants host and port only.
	if i := strings.IndexByte(trimmed, '/'); i >= 0 {
		trimmed = trimmed[:i]
	}
	if trimmed == "" {
		return "", false, false
	}
	return trimmed, insecure, true
}

func (f exporterFactory) traces(ctx context.Context) (sdktrace.SpanExporter, error) {
	if f.protocol == ProtocolHTTP {
		o := []otlptracehttp.Option{otlptracehttp.WithCompression(otlptracehttp.GzipCompression)}
		// WithEndpoint takes host:port and appends the signal path
		// ("/v1/traces"). WithEndpointURL would use the path as-is, which
		// posts to "/" and gets a 404 from a real OTLP server.
		if host, insecure, ok := f.hostPort(); ok {
			o = append(o, otlptracehttp.WithEndpoint(host))
			if insecure {
				o = append(o, otlptracehttp.WithInsecure())
			}
		}
		if len(f.headers) > 0 {
			o = append(o, otlptracehttp.WithHeaders(f.headers))
		}
		return otlptracehttp.New(ctx, o...)
	}

	return otlptracegrpc.New(ctx,
		otlptracegrpc.WithGRPCConn(f.conn),
		otlptracegrpc.WithHeaders(f.headers),
		otlptracegrpc.WithCompressor("gzip"),
	)
}

func (f exporterFactory) metrics(ctx context.Context) (sdkmetric.Exporter, error) {
	if f.protocol == ProtocolHTTP {
		o := []otlpmetrichttp.Option{
			otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression),
			// The two settings New Relic requires and the SDK does not default to.
			otlpmetrichttp.WithTemporalitySelector(deltaTemporalitySelector),
			otlpmetrichttp.WithAggregationSelector(exponentialHistogramSelector),
		}
		if host, insecure, ok := f.hostPort(); ok {
			o = append(o, otlpmetrichttp.WithEndpoint(host))
			if insecure {
				o = append(o, otlpmetrichttp.WithInsecure())
			}
		}
		if len(f.headers) > 0 {
			o = append(o, otlpmetrichttp.WithHeaders(f.headers))
		}
		return otlpmetrichttp.New(ctx, o...)
	}

	return otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithGRPCConn(f.conn),
		otlpmetricgrpc.WithHeaders(f.headers),
		otlpmetricgrpc.WithCompressor("gzip"),
		otlpmetricgrpc.WithTemporalitySelector(deltaTemporalitySelector),
		otlpmetricgrpc.WithAggregationSelector(exponentialHistogramSelector),
	)
}

func (f exporterFactory) logs(ctx context.Context) (sdklog.Exporter, error) {
	if f.protocol == ProtocolHTTP {
		o := []otlploghttp.Option{otlploghttp.WithCompression(otlploghttp.GzipCompression)}
		if host, insecure, ok := f.hostPort(); ok {
			o = append(o, otlploghttp.WithEndpoint(host))
			if insecure {
				o = append(o, otlploghttp.WithInsecure())
			}
		}
		if len(f.headers) > 0 {
			o = append(o, otlploghttp.WithHeaders(f.headers))
		}
		return otlploghttp.New(ctx, o...)
	}

	return otlploggrpc.New(ctx,
		otlploggrpc.WithGRPCConn(f.conn),
		otlploggrpc.WithHeaders(f.headers),
		otlploggrpc.WithCompressor("gzip"),
	)
}

// all builds the three exporters, naming the service in any error.
func (f exporterFactory) all(ctx context.Context, service string) (sdktrace.SpanExporter, sdkmetric.Exporter, sdklog.Exporter, error) {
	traceExp, err := f.traces(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating trace exporter for %s: %w", service, err)
	}
	metricExp, err := f.metrics(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating metric exporter for %s: %w", service, err)
	}
	logExp, err := f.logs(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating log exporter for %s: %w", service, err)
	}
	return traceExp, metricExp, logExp, nil
}
