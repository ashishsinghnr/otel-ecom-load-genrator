// Command otel-ecom-load-genrator emits synthetic OpenTelemetry traces,
// metrics, and logs for an e-commerce topology described in JSON.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof" // registers pprof handlers on the default mux
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/engine"
	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/telemetry"
)

// shutdownTimeout bounds the final flush so a wedged backend cannot hang exit.
const shutdownTimeout = 10 * time.Second

type flags struct {
	topology     string
	backend      string
	endpoint     string
	protocol     string
	nrRegion     string
	namespace    string
	environment  string
	timing       string
	duration     time.Duration
	reportEvery  time.Duration
	validateOnly bool
	pprofAddr    string
	logLevel     string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	f := parseFlags()

	logger, err := newLogger(f.logLevel)
	if err != nil {
		return err
	}

	if f.topology == "" {
		return errors.New("--topology is required")
	}

	// Load and validate before building any exporter, so an invalid topology
	// is a startup error rather than silently-missing telemetry.
	topoFile, warnings, err := config.Load(f.topology)
	for _, w := range warnings {
		logger.Warn("topology warning", "detail", w)
	}
	if err != nil {
		return err
	}

	if f.validateOnly {
		logger.Info("topology is valid",
			"services", len(topoFile.Topology.Services),
			"rootRoutes", len(topoFile.RootRoutes),
			"warnings", len(warnings),
		)
		return nil
	}

	backend, err := telemetry.ParseBackend(f.backend)
	if err != nil {
		return err
	}
	protocol, err := telemetry.ParseProtocol(f.protocol)
	if err != nil {
		return err
	}
	timing, err := engine.ParseTimingMode(f.timing)
	if err != nil {
		return err
	}

	// Route the SDK's internal errors through our logger. Without this they go
	// to the standard logger with no level or context, so a backend problem
	// shows up as bare "rpc error" lines.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Warn("otel sdk error", "error", firstLine(err.Error()))
	}))

	if f.pprofAddr != "" {
		startPprof(f.pprofAddr, logger)
	}

	// Signal handling covers SIGTERM as well as SIGINT, which matters under
	// Docker and Kubernetes.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	providers, err := telemetry.Setup(ctx, topoFile.Topology, telemetry.Options{
		Backend:     backend,
		Endpoint:    f.endpoint,
		Protocol:    protocol,
		NRRegion:    f.nrRegion,
		Namespace:   f.namespace,
		Environment: f.environment,
	})
	if err != nil {
		return err
	}

	eng := engine.New(topoFile.Topology, providers, engine.Options{
		Timing: timing,
		Logger: logger,
	})

	totalRate := 0
	for _, rr := range topoFile.RootRoutes {
		totalRate += rr.TracesPerHour
	}
	logger.Info("starting load generator",
		"backend", backend,
		"endpoint", telemetry.DescribeEndpoint(telemetry.Options{
			Backend: backend, Endpoint: f.endpoint, Protocol: protocol, NRRegion: f.nrRegion,
		}),
		"protocol", protocol,
		"services", len(topoFile.Topology.Services),
		"rootRoutes", len(topoFile.RootRoutes),
		"tracesPerHour", totalRate,
		"timing", timing,
	)

	runCtx := ctx
	if f.duration > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, f.duration)
		defer cancel()
		logger.Info("running for a fixed duration", "duration", f.duration)
	}

	eng.Run(runCtx, topoFile.RootRoutes, topoFile.Surges, f.reportEvery)

	// Flush on a fresh context: the run context is already cancelled, and a
	// cancelled context would abort the flush that is the point of shutdown.
	logger.Info("shutting down, flushing telemetry", "timeout", shutdownTimeout)
	flushCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// A failed flush is reported but does not fail the run. The load was
	// generated; losing the final batch to a backend problem is a warning, not
	// a reason to exit non-zero. Startup problems (a bad topology, a missing
	// license key) already exited before this point.
	if err := providers.Shutdown(flushCtx); err != nil {
		logger.Warn("some telemetry could not be flushed on shutdown",
			"hint", "check the endpoint, credentials and network",
			"error", firstLine(err.Error()),
		)
	}

	st := eng.Stats()
	logger.Info("run summary",
		"traces", st.Traces,
		"spans", st.Spans,
		"logs", st.Logs,
		"errorSpans", st.ErrorSpans,
		"panics", st.Panics,
	)
	if st.Panics > 0 {
		logger.Warn("recovered emission panics during the run", "count", st.Panics)
	}
	logger.Info("shutdown complete")
	return nil
}

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.topology, "topology", "", "Path to the topology JSON file (required)")
	flag.StringVar(&f.backend, "backend", "newrelic", "Exporter defaults: newrelic, otlp or local")
	flag.StringVar(&f.endpoint, "endpoint", "", "OTLP endpoint URL or host:port (overrides --backend default and OTEL_EXPORTER_OTLP_ENDPOINT)")
	flag.StringVar(&f.protocol, "protocol", "grpc", "OTLP protocol: grpc (port 4317) or http (port 4318)")
	flag.StringVar(&f.nrRegion, "nr-region", "us", "New Relic region: us or eu")
	flag.StringVar(&f.namespace, "namespace", "ecom", "service.namespace resource attribute")
	flag.StringVar(&f.environment, "environment", "synthetic", "deployment.environment.name resource attribute")
	flag.StringVar(&f.timing, "timing", "synthetic", "Span timing: synthetic or nested")
	flag.DurationVar(&f.duration, "duration", 0, "Stop after this long (0 runs until interrupted)")
	flag.DurationVar(&f.reportEvery, "report-every", 30*time.Second, "Log a progress line at this interval (0 disables)")
	flag.BoolVar(&f.validateOnly, "validate-only", false, "Validate the topology and exit")
	flag.StringVar(&f.pprofAddr, "pprof", "", "pprof listen address, e.g. localhost:6060 (empty disables it)")
	flag.StringVar(&f.logLevel, "log-level", "info", "Log level: debug, info, warn or error")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"otel-ecom-load-genrator emits synthetic OTel traces, metrics and logs\n"+
				"for the e-commerce topology described in a JSON file.\n\n"+
				"Usage:\n  %s --topology topologies/shop-full.json\n\nOptions:\n",
			os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(),
			"\nEnvironment:\n"+
				"  NEW_RELIC_LICENSE_KEY   Required when --backend=newrelic\n"+
				"  OTEL_EXPORTER_OTLP_*    Standard OTLP variables; an explicit\n"+
				"                          endpoint overrides the --backend default\n")
	}
	flag.Parse()
	return f
}

// firstLine returns the first line of a message, with a count of how many
// more there are.
//
// Shutdown joins one error per provider, so a single backend problem produces
// the same message once per simulated service. Logging all of them buries the
// signal.
func firstLine(msg string) string {
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	if len(lines) <= 1 {
		return msg
	}
	return fmt.Sprintf("%s (and %d more)", lines[0], len(lines)-1)
}

func newLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", level)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(logger)
	return logger, nil
}

// startPprof serves profiling endpoints. Failures are logged rather than
// fatal: profiling is a diagnostic aid, not part of the job.
func startPprof(addr string, logger *slog.Logger) {
	srv := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("pprof listening", "address", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof server stopped", "error", err)
		}
	}()
}
