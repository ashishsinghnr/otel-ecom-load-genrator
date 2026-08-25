package telemetry

import (
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestParseBackend(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Backend
	}{
		{"newrelic", BackendNewRelic},
		{"NewRelic", BackendNewRelic},
		{" otlp ", BackendOTLP},
		{"local", BackendLocal},
	} {
		got, err := ParseBackend(tc.in)
		if err != nil {
			t.Fatalf("ParseBackend(%q) errored: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseBackend(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if _, err := ParseBackend("splunk"); err == nil {
		t.Error("expected unknown backend to error")
	}
}

func TestResolveEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	tests := []struct {
		name string
		o    Options
		want string
	}{
		{"newrelic default region", Options{Backend: BackendNewRelic}, newRelicUSEndpoint},
		{"newrelic us", Options{Backend: BackendNewRelic, NRRegion: "us"}, newRelicUSEndpoint},
		{"newrelic eu", Options{Backend: BackendNewRelic, NRRegion: "EU"}, newRelicEUEndpoint},
		{"local", Options{Backend: BackendLocal}, localEndpoint},
		{"otlp defers to sdk", Options{Backend: BackendOTLP}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.o.resolveEndpoint()
			if err != nil {
				t.Fatalf("resolveEndpoint errored: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveEndpoint = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("unknown region errors", func(t *testing.T) {
		_, err := Options{Backend: BackendNewRelic, NRRegion: "mars"}.resolveEndpoint()
		if err == nil {
			t.Fatal("expected unknown region to error")
		}
	})
}

// The US endpoint must point at New Relic staging. Synthetic load belongs in
// staging, so a change back to production should be deliberate and visible.
func TestResolveEndpoint_USTargetsStaging(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	got, err := Options{Backend: BackendNewRelic, NRRegion: "us"}.resolveEndpoint()
	if err != nil {
		t.Fatalf("resolveEndpoint errored: %v", err)
	}
	const want = "https://staging-otlp.nr-data.net:4317"
	if got != want {
		t.Errorf("US endpoint = %q, want %q", got, want)
	}
}

// An explicit OTLP endpoint must win over the backend default, so the tool
// stays usable with any collector.
func TestResolveEndpoint_EnvWins(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4317")
	got, err := Options{Backend: BackendNewRelic}.resolveEndpoint()
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if got != "" {
		t.Errorf("resolveEndpoint = %q, want empty so the SDK reads the env var", got)
	}
}

func TestHeaders_NewRelicRequiresLicenseKey(t *testing.T) {
	t.Setenv(licenseKeyEnv, "")
	_, err := Options{Backend: BackendNewRelic}.headers()
	if err == nil {
		t.Fatal("expected missing license key to error")
	}
	if !strings.Contains(err.Error(), licenseKeyEnv) {
		t.Errorf("error %q should name the env var", err.Error())
	}
}

func TestHeaders_NewRelicSetsApiKey(t *testing.T) {
	t.Setenv(licenseKeyEnv, "secret-key-value")
	h, err := Options{Backend: BackendNewRelic}.headers()
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	if h["api-key"] != "secret-key-value" {
		t.Errorf("api-key header = %q, want the license key", h["api-key"])
	}
}

func TestHeaders_OtherBackendsNeedNoKey(t *testing.T) {
	t.Setenv(licenseKeyEnv, "")
	for _, b := range []Backend{BackendOTLP, BackendLocal} {
		h, err := Options{Backend: b}.headers()
		if err != nil {
			t.Errorf("backend %q errored without a license key: %v", b, err)
		}
		if len(h) != 0 {
			t.Errorf("backend %q set headers %v, want none", b, h)
		}
	}
}

// Delta temporality is the setting New Relic requires and the SDK does not
// default to. Guard it explicitly.
func TestDeltaTemporalitySelector(t *testing.T) {
	delta := []sdkmetric.InstrumentKind{
		sdkmetric.InstrumentKindCounter,
		sdkmetric.InstrumentKindHistogram,
		sdkmetric.InstrumentKindObservableCounter,
		sdkmetric.InstrumentKindObservableUpDownCounter,
	}
	for _, k := range delta {
		if got := deltaTemporalitySelector(k); got != metricdata.DeltaTemporality {
			t.Errorf("kind %v -> %v, want delta", k, got)
		}
	}

	// UpDownCounter must stay cumulative: delta is meaningless for a value
	// that can decrease.
	if got := deltaTemporalitySelector(sdkmetric.InstrumentKindUpDownCounter); got != metricdata.CumulativeTemporality {
		t.Errorf("UpDownCounter -> %v, want cumulative", got)
	}
}

func TestExponentialHistogramSelector(t *testing.T) {
	agg := exponentialHistogramSelector(sdkmetric.InstrumentKindHistogram)
	expo, ok := agg.(sdkmetric.AggregationBase2ExponentialHistogram)
	if !ok {
		t.Fatalf("histogram aggregation = %T, want AggregationBase2ExponentialHistogram", agg)
	}
	if expo.MaxSize <= 0 {
		t.Errorf("MaxSize = %d, want positive", expo.MaxSize)
	}
	if expo.MaxScale > 20 {
		t.Errorf("MaxScale = %d, exceeds the SDK maximum of 20", expo.MaxScale)
	}

	// Counters must keep their default aggregation.
	if _, ok := exponentialHistogramSelector(sdkmetric.InstrumentKindCounter).(sdkmetric.AggregationBase2ExponentialHistogram); ok {
		t.Error("counter should not use exponential histogram aggregation")
	}
}

func TestParseProtocol(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Protocol
	}{
		{"grpc", ProtocolGRPC},
		{"GRPC", ProtocolGRPC},
		{"http", ProtocolHTTP},
		{" http/protobuf ", ProtocolHTTP},
	} {
		got, err := ParseProtocol(tc.in)
		if err != nil {
			t.Fatalf("ParseProtocol(%q) errored: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseProtocol(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	if _, err := ParseProtocol("thrift"); err == nil {
		t.Error("expected unknown protocol to error")
	}
}

// The protocol must select the conventional port: 4317 for gRPC, 4318 for HTTP.
// Sending gRPC traffic to 4318 fails, so this mapping matters.
func TestResolveEndpoint_ProtocolSelectsPort(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	tests := []struct {
		name string
		o    Options
		want string
	}{
		{"newrelic grpc", Options{Backend: BackendNewRelic, Protocol: ProtocolGRPC}, "https://staging-otlp.nr-data.net:4317"},
		{"newrelic http", Options{Backend: BackendNewRelic, Protocol: ProtocolHTTP}, "https://staging-otlp.nr-data.net:4318"},
		{"newrelic default is grpc", Options{Backend: BackendNewRelic}, "https://staging-otlp.nr-data.net:4317"},
		{"local grpc", Options{Backend: BackendLocal, Protocol: ProtocolGRPC}, "http://localhost:4317"},
		{"local http", Options{Backend: BackendLocal, Protocol: ProtocolHTTP}, "http://localhost:4318"},
		{"eu http", Options{Backend: BackendNewRelic, NRRegion: "eu", Protocol: ProtocolHTTP}, "https://otlp.eu01.nr-data.net:4318"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.o.resolveEndpoint()
			if err != nil {
				t.Fatalf("errored: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveEndpoint = %q, want %q", got, tc.want)
			}
		})
	}
}

// The --endpoint flag must take precedence over both the backend default and
// the environment variable, so a run can be pointed anywhere without code
// changes or unsetting the environment.
func TestResolveEndpoint_FlagPrecedence(t *testing.T) {
	t.Run("flag beats backend default", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		got, err := Options{
			Backend:  BackendNewRelic,
			Endpoint: "https://my-collector:4318",
		}.resolveEndpoint()
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		if got != "https://my-collector:4318" {
			t.Errorf("resolveEndpoint = %q, want the flag value", got)
		}
	})

	t.Run("flag beats env var", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://from-env:4318")
		got, err := Options{
			Backend:  BackendNewRelic,
			Endpoint: "https://from-flag:4318",
		}.resolveEndpoint()
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		if got != "https://from-flag:4318" {
			t.Errorf("resolveEndpoint = %q, want the flag to win over the env var", got)
		}
	})

	t.Run("env var used when no flag", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://from-env:4318")
		got, err := Options{Backend: BackendNewRelic}.resolveEndpoint()
		if err != nil {
			t.Fatalf("errored: %v", err)
		}
		// Empty means "let the SDK read the env var itself".
		if got != "" {
			t.Errorf("resolveEndpoint = %q, want empty so the SDK reads the env var", got)
		}
	})
}

// DescribeEndpoint must name which source won, so a run's log is unambiguous.
func TestDescribeEndpoint(t *testing.T) {
	t.Run("flag", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		got := DescribeEndpoint(Options{Backend: BackendNewRelic, Endpoint: "https://x:4318"})
		if !strings.Contains(got, "https://x:4318") || !strings.Contains(got, "--endpoint") {
			t.Errorf("DescribeEndpoint = %q, want it to name the flag and value", got)
		}
	})

	t.Run("env", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://env-host:4318")
		got := DescribeEndpoint(Options{Backend: BackendNewRelic})
		if !strings.Contains(got, "env-host") || !strings.Contains(got, "OTEL_EXPORTER_OTLP_ENDPOINT") {
			t.Errorf("DescribeEndpoint = %q, want it to name the env var", got)
		}
	})

	t.Run("backend default", func(t *testing.T) {
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
		got := DescribeEndpoint(Options{Backend: BackendNewRelic, Protocol: ProtocolHTTP})
		if !strings.Contains(got, "staging-otlp.nr-data.net:4318") {
			t.Errorf("DescribeEndpoint = %q, want the resolved staging URL with the HTTP port", got)
		}
		if !strings.Contains(got, "--backend") {
			t.Errorf("DescribeEndpoint = %q, want it to name the backend as the source", got)
		}
	})
}

// gRPC must honor the flag too, including stripping a path.
func TestGRPCTarget_HonorsFlagAndStripsPath(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	opts := Options{Backend: BackendNewRelic, Endpoint: "https://host.example:4317/v1/traces"}
	ep, err := opts.resolveEndpoint()
	if err != nil {
		t.Fatalf("errored: %v", err)
	}
	target, useTLS := grpcTarget(ep, opts)
	if target != "host.example:4317" {
		t.Errorf("target = %q, want host.example:4317 with scheme and path stripped", target)
	}
	if !useTLS {
		t.Error("https endpoint should use TLS")
	}
}
