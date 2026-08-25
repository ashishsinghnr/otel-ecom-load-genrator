package telemetry

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	_ "google.golang.org/grpc/encoding/gzip" // registers the gzip compressor

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
)

// instrumentationScope names this generator as the producer of the telemetry.
const instrumentationScope = "github.com/ashishsinghnr/otel-ecom-load-genrator"

// metricExportInterval is how often each service's metrics are pushed.
const metricExportInterval = 10 * time.Second

// Providers owns every SDK provider and the per-service emitters.
//
// One provider trio per service is what makes a single process appear as many
// distinct services to the backend: each carries its own resource.
type Providers struct {
	emitters map[string]*Emitter

	traceProviders  []*sdktrace.TracerProvider
	metricProviders []*sdkmetric.MeterProvider
	logProviders    []*sdklog.LoggerProvider

	// conn is the single gRPC connection shared by every exporter. It is
	// closed after all providers have shut down.
	conn *grpc.ClientConn
}

// Emitter returns the emitter for a service, or nil when unknown.
func (p *Providers) Emitter(service string) *Emitter {
	return p.emitters[service]
}

// Setup builds one provider trio and emitter per service in the topology.
//
// All exporters share one gRPC connection, so a large topology costs one
// connection rather than three per service. Each service still gets its own
// exporter instance: an exporter's Shutdown closes its transport, so sharing
// exporter instances across providers would make the first provider's
// shutdown break every other provider's flush.
func Setup(ctx context.Context, topo config.Topology, opts Options) (*Providers, error) {
	endpoint, err := opts.resolveEndpoint()
	if err != nil {
		return nil, err
	}
	headers, err := opts.headers()
	if err != nil {
		return nil, err
	}

	p := &Providers{emitters: make(map[string]*Emitter, len(topo.Services))}

	factory := exporterFactory{
		protocol: opts.protocol(),
		headers:  headers,
		endpoint: endpoint,
		insecure: opts.Backend == BackendLocal,
	}

	// gRPC shares one connection across every service's exporters. HTTP
	// exporters manage their own transport, so no dial is needed.
	if factory.protocol == ProtocolGRPC {
		conn, err := dialGRPC(ctx, endpoint, opts)
		if err != nil {
			return nil, fmt.Errorf("connecting to the OTLP endpoint: %w", err)
		}
		p.conn = conn
		factory.conn = conn
	}

	for _, svc := range topo.Services {
		if err := p.addService(ctx, svc, opts, factory); err != nil {
			// Clean up whatever was already built so a partial failure does
			// not leak connections or goroutines.
			_ = p.Shutdown(context.Background())
			return nil, err
		}
	}

	return p, nil
}

// addService builds and registers one service's providers and emitter.
func (p *Providers) addService(ctx context.Context, svc config.Service, opts Options, factory exporterFactory) error {
	res, err := buildResource(ctx, svc, opts)
	if err != nil {
		return fmt.Errorf("building resource for %s: %w", svc.ServiceName, err)
	}

	traceExp, metricExp, logExp, err := factory.all(ctx, svc.ServiceName)
	if err != nil {
		return err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(traceExp),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp,
			sdkmetric.WithInterval(metricExportInterval),
		)),
	)
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
	)

	em, err := newEmitter(
		svc.ServiceName,
		tp.Tracer(instrumentationScope),
		mp.Meter(instrumentationScope),
		lp.Logger(instrumentationScope),
	)
	if err != nil {
		return err
	}

	p.emitters[svc.ServiceName] = em
	p.traceProviders = append(p.traceProviders, tp)
	p.metricProviders = append(p.metricProviders, mp)
	p.logProviders = append(p.logProviders, lp)
	return nil
}

// dialGRPC opens the single connection every exporter shares.
//
// An http:// endpoint or the local backend selects an insecure connection;
// anything else uses TLS, which is what New Relic requires.
func dialGRPC(ctx context.Context, endpoint string, opts Options) (*grpc.ClientConn, error) {
	target, useTLS := grpcTarget(endpoint, opts)

	creds := credentials.NewClientTLSFromCert(nil, "")
	var transport grpc.DialOption = grpc.WithTransportCredentials(creds)
	if !useTLS {
		transport = grpc.WithTransportCredentials(insecure.NewCredentials())
	}

	return grpc.NewClient(target, transport)
}

// buildResource describes one simulated service to the backend.
func buildResource(ctx context.Context, svc config.Service, opts Options) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(svc.ServiceName),
		semconv.ServiceNamespace(opts.Namespace),
		semconv.DeploymentEnvironmentNameKey.String(opts.Environment),
	}
	if svc.Tier != "" {
		attrs = append(attrs, attribute.String("service.tier", svc.Tier))
	}

	return resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithTelemetrySDK(),
	)
}

// Shutdown flushes and stops every provider, then closes the shared
// connection.
//
// Both a flush and a shutdown are required: returning without flushing
// discards whatever the batch processors are still holding (spec C8). Errors
// are collected rather than returned early so that one failing provider does
// not prevent the others from flushing.
func (p *Providers) Shutdown(ctx context.Context) error {
	// Flush everything before shutting anything down, so a slow provider
	// cannot cause a faster one's batch to be dropped.
	errs := p.forEachProvider(ctx, "flushing",
		func(ctx context.Context, tp *sdktrace.TracerProvider) error { return tp.ForceFlush(ctx) },
		func(ctx context.Context, mp *sdkmetric.MeterProvider) error { return mp.ForceFlush(ctx) },
		func(ctx context.Context, lp *sdklog.LoggerProvider) error { return lp.ForceFlush(ctx) },
	)
	errs = append(errs, p.forEachProvider(ctx, "shutting down",
		func(ctx context.Context, tp *sdktrace.TracerProvider) error { return tp.Shutdown(ctx) },
		func(ctx context.Context, mp *sdkmetric.MeterProvider) error { return mp.Shutdown(ctx) },
		func(ctx context.Context, lp *sdklog.LoggerProvider) error { return lp.Shutdown(ctx) },
	)...)

	// The connection closes last: the exporters above still need it to flush.
	if p.conn != nil {
		if err := p.conn.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing the gRPC connection: %w", err))
		}
	}

	return errors.Join(errs...)
}

// forEachProvider applies one operation to every provider, collecting errors
// rather than stopping early so that one failure does not strand the rest.
func (p *Providers) forEachProvider(
	ctx context.Context,
	op string,
	traceFn func(context.Context, *sdktrace.TracerProvider) error,
	metricFn func(context.Context, *sdkmetric.MeterProvider) error,
	logFn func(context.Context, *sdklog.LoggerProvider) error,
) []error {
	var errs []error
	for _, tp := range p.traceProviders {
		if err := traceFn(ctx, tp); err != nil {
			errs = append(errs, fmt.Errorf("%s traces: %w", op, err))
		}
	}
	for _, mp := range p.metricProviders {
		if err := metricFn(ctx, mp); err != nil {
			errs = append(errs, fmt.Errorf("%s metrics: %w", op, err))
		}
	}
	for _, lp := range p.logProviders {
		if err := logFn(ctx, lp); err != nil {
			errs = append(errs, fmt.Errorf("%s logs: %w", op, err))
		}
	}
	return errs
}
