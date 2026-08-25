// Package telemetry builds OTLP providers and exposes an Emitter that groups
// one simulated service's tracer, meter, and logger behind a single resource.
package telemetry

import (
	"fmt"
	"os"
	"strings"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Backend selects exporter defaults.
type Backend string

const (
	// BackendNewRelic targets New Relic's OTLP endpoint. It requires an
	// api-key header and delta temporality.
	BackendNewRelic Backend = "newrelic"
	// BackendOTLP honors only the standard OTEL_EXPORTER_OTLP_* variables.
	BackendOTLP Backend = "otlp"
	// BackendLocal targets an insecure collector on localhost.
	BackendLocal Backend = "local"
)

// Protocol selects the OTLP wire protocol.
type Protocol string

const (
	// ProtocolGRPC is OTLP over gRPC, conventionally port 4317.
	ProtocolGRPC Protocol = "grpc"
	// ProtocolHTTP is OTLP over HTTP/protobuf, conventionally port 4318.
	ProtocolHTTP Protocol = "http"
)

// ParseProtocol converts a flag value to a Protocol.
//
// The standard OTEL_EXPORTER_OTLP_PROTOCOL spelling "http/protobuf" is
// accepted, since that is what other OTel tooling uses.
func ParseProtocol(s string) (Protocol, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "grpc":
		return ProtocolGRPC, nil
	case "http", "http/protobuf":
		return ProtocolHTTP, nil
	default:
		return "", fmt.Errorf("unknown OTLP protocol %q (want grpc or http)", s)
	}
}

// defaultPortFor returns the conventional OTLP port for a protocol.
func defaultPortFor(p Protocol) string {
	if p == ProtocolHTTP {
		return "4318"
	}
	return "4317"
}

// New Relic OTLP hosts by region. The port is appended per protocol, since
// gRPC and HTTP use different ones (4317 and 4318).
//
// The US host is New Relic's staging environment, which is where this
// generator's synthetic load belongs: it is test data, and sending it to a
// production account would pollute real dashboards and consume real ingest.
// To target production, change newRelicUSHost to otlp.nr-data.net.
const (
	newRelicUSHost = "staging-otlp.nr-data.net"
	newRelicEUHost = "otlp.eu01.nr-data.net"
	localHost      = "localhost"
)

// Endpoint constants retained for tests and documentation. These are the
// gRPC-port forms; resolveEndpoint builds the protocol-correct URL.
const (
	newRelicUSEndpoint = "https://" + newRelicUSHost + ":4317"
	newRelicEUEndpoint = "https://" + newRelicEUHost + ":4317"
	localEndpoint      = "http://" + localHost + ":4317"
)

// licenseKeyEnv holds the New Relic ingest key. It is read from the
// environment only: never a flag, since flags leak into shell history and
// process listings.
const licenseKeyEnv = "NEW_RELIC_LICENSE_KEY"

// Options configures provider construction.
type Options struct {
	Backend     Backend
	Protocol    Protocol // grpc (4317) or http (4318); defaults to grpc
	NRRegion    string   // "us" or "eu"; only used when Backend is newrelic
	Namespace   string
	Environment string

	// endpointOverride forces a specific endpoint, bypassing backend defaults
	// and the environment. Unexported so it is only settable from this package,
	// which is what tests need to point at a local server.
	endpointOverride string
}

// protocol returns the configured protocol, defaulting to gRPC.
func (o Options) protocol() Protocol {
	if o.Protocol == ProtocolHTTP {
		return ProtocolHTTP
	}
	return ProtocolGRPC
}

// ParseBackend converts a flag value to a Backend.
func ParseBackend(s string) (Backend, error) {
	switch Backend(strings.ToLower(strings.TrimSpace(s))) {
	case BackendNewRelic:
		return BackendNewRelic, nil
	case BackendOTLP:
		return BackendOTLP, nil
	case BackendLocal:
		return BackendLocal, nil
	default:
		return "", fmt.Errorf("unknown backend %q (want newrelic, otlp or local)", s)
	}
}

// resolveEndpoint returns the endpoint for the backend, or "" to mean
// "let the SDK read OTEL_EXPORTER_OTLP_ENDPOINT".
//
// An explicit OTEL_EXPORTER_OTLP_ENDPOINT always wins, so the tool stays
// usable with any OTLP consumer regardless of --backend.
func (o Options) resolveEndpoint() (string, error) {
	if o.endpointOverride != "" {
		return o.endpointOverride, nil
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		return "", nil
	}

	port := defaultPortFor(o.protocol())

	switch o.Backend {
	case BackendNewRelic:
		switch strings.ToLower(o.NRRegion) {
		case "", "us":
			return "https://" + newRelicUSHost + ":" + port, nil
		case "eu":
			return "https://" + newRelicEUHost + ":" + port, nil
		default:
			return "", fmt.Errorf("unknown New Relic region %q (want us or eu)", o.NRRegion)
		}
	case BackendLocal:
		return "http://" + localHost + ":" + port, nil
	default:
		return "", nil
	}
}

// grpcTarget converts an endpoint into a gRPC dial target and reports whether
// TLS should be used.
//
// gRPC dials a host:port, not a URL, so any scheme is stripped. The scheme
// also selects the transport: http:// means insecure, https:// means TLS, and
// a bare host:port follows the OTLP default of TLS unless the backend is the
// local collector.
func grpcTarget(endpoint string, opts Options) (target string, useTLS bool) {
	// Fall back to the standard env var, then to the backend default.
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if endpoint == "" {
		if opts.Backend == BackendLocal {
			endpoint = localEndpoint
		} else {
			endpoint = newRelicUSEndpoint
		}
	}

	switch {
	case strings.HasPrefix(endpoint, "http://"):
		return strings.TrimPrefix(endpoint, "http://"), false
	case strings.HasPrefix(endpoint, "https://"):
		return strings.TrimPrefix(endpoint, "https://"), true
	default:
		// No scheme: OTLP defaults to secure, except for the local collector.
		return endpoint, opts.Backend != BackendLocal
	}
}

// headers returns exporter headers for the backend.
func (o Options) headers() (map[string]string, error) {
	if o.Backend != BackendNewRelic {
		return nil, nil
	}
	key := os.Getenv(licenseKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("%s is not set; required when --backend=newrelic", licenseKeyEnv)
	}
	return map[string]string{"api-key": key}, nil
}

// deltaTemporalitySelector maps instrument kinds to delta temporality.
//
// New Relic requires delta temporality for counters and histograms. The SDK
// default is cumulative, which is accepted but produces wrong values, so this
// must be set explicitly rather than left alone.
func deltaTemporalitySelector(k sdkmetric.InstrumentKind) metricdata.Temporality {
	switch k {
	case sdkmetric.InstrumentKindCounter,
		sdkmetric.InstrumentKindHistogram,
		sdkmetric.InstrumentKindObservableCounter,
		sdkmetric.InstrumentKindObservableUpDownCounter:
		return metricdata.DeltaTemporality
	default:
		return metricdata.CumulativeTemporality
	}
}

// exponentialHistogramSelector prefers base-2 exponential histograms, which
// New Relic recommends for long-tailed distributions such as request duration.
func exponentialHistogramSelector(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	if k == sdkmetric.InstrumentKindHistogram {
		return sdkmetric.AggregationBase2ExponentialHistogram{
			MaxSize:  160,
			MaxScale: 20,
		}
	}
	return sdkmetric.DefaultAggregationSelector(k)
}
