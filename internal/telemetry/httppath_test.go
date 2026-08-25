package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
)

// strictOTLPServer accepts only the canonical OTLP HTTP paths and returns 404
// for anything else, the way a real OTLP server does.
//
// This strictness is the point. A permissive test server that accepts any path
// hides the failure this test exists to catch: the SDK's WithEndpointURL uses
// the URL path as-is and does NOT append "/v1/traces", so passing a bare
// host:port URL posts to "/" and a real server answers 404.
type strictOTLPServer struct {
	mu       sync.Mutex
	accepted map[string]int
	rejected map[string]int
}

func newStrictOTLPServer() (*strictOTLPServer, *httptest.Server) {
	s := &strictOTLPServer{
		accepted: map[string]int{},
		rejected: map[string]int{},
	}

	valid := map[string]bool{
		"/v1/traces":  true,
		"/v1/metrics": true,
		"/v1/logs":    true,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()

		if !valid[r.URL.Path] {
			s.rejected[r.URL.Path]++
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.accepted[r.URL.Path]++
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))

	return s, srv
}

func (s *strictOTLPServer) snapshot() (accepted, rejected map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	accepted = map[string]int{}
	rejected = map[string]int{}
	for k, v := range s.accepted {
		accepted[k] = v
	}
	for k, v := range s.rejected {
		rejected[k] = v
	}
	return accepted, rejected
}

// HTTP exporters must post to the canonical signal paths. Regression test for
// a 404 caused by posting to "/" instead of "/v1/traces".
func TestHTTPExporters_PostToCanonicalPaths(t *testing.T) {
	sink, srv := newStrictOTLPServer()
	defer srv.Close()

	// httptest serves plain HTTP; the http:// scheme selects insecure.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	ctx := context.Background()
	topo := config.Topology{Services: []config.Service{{
		ServiceName: "svc",
		Instances:   []string{"svc-1"},
		Routes: []config.Route{{
			Route:   "GET /x",
			Latency: config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1},
		}},
	}}}

	providers, err := Setup(ctx, topo, Options{
		Backend:  BackendOTLP,
		Protocol: ProtocolHTTP,
		// srv.URL is http://127.0.0.1:PORT with no path.
		endpointOverride: srv.URL,
		Namespace:        "test",
		Environment:      "test",
	})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	em := providers.Emitter("svc")
	if em == nil {
		t.Fatal("no emitter")
	}

	spanCtx, span := em.Tracer().Start(ctx, "GET /x")
	em.RecordRED(spanCtx, "GET /x", 12, 200, false)
	em.Log(spanCtx, 9, "INFO", "hello")
	span.End()

	flushCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := providers.Shutdown(flushCtx); err != nil {
		t.Fatalf("Shutdown reported errors, which means the server rejected the requests: %v", err)
	}

	accepted, rejected := sink.snapshot()

	if len(rejected) > 0 {
		t.Errorf("requests were sent to non-OTLP paths (each would be a 404 in production): %v", rejected)
	}
	for _, want := range []string{"/v1/traces", "/v1/metrics", "/v1/logs"} {
		if accepted[want] == 0 {
			t.Errorf("no requests reached %s; accepted=%v rejected=%v", want, accepted, rejected)
		}
	}
}

// hostPort must reduce a resolved endpoint to host:port for WithEndpoint,
// which is what makes the SDK append the signal path.
func TestHostPort(t *testing.T) {
	tests := []struct {
		name         string
		f            exporterFactory
		wantHost     string
		wantInsecure bool
		wantOK       bool
	}{
		{
			name:     "https url",
			f:        exporterFactory{endpoint: "https://staging-otlp.nr-data.net:4318"},
			wantHost: "staging-otlp.nr-data.net:4318",
			wantOK:   true,
		},
		{
			name:         "http url is insecure",
			f:            exporterFactory{endpoint: "http://localhost:4318"},
			wantHost:     "localhost:4318",
			wantInsecure: true,
			wantOK:       true,
		},
		{
			name:     "path is dropped",
			f:        exporterFactory{endpoint: "https://example.com:4318/v1/traces"},
			wantHost: "example.com:4318",
			wantOK:   true,
		},
		{
			name:     "bare host port",
			f:        exporterFactory{endpoint: "example.com:4318"},
			wantHost: "example.com:4318",
			wantOK:   true,
		},
		{
			name:   "empty defers to the sdk",
			f:      exporterFactory{endpoint: ""},
			wantOK: false,
		},
		{
			name:         "insecure flag is honored without a scheme",
			f:            exporterFactory{endpoint: "localhost:4318", insecure: true},
			wantHost:     "localhost:4318",
			wantInsecure: true,
			wantOK:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, insecure, ok := tc.f.hostPort()
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
			if insecure != tc.wantInsecure {
				t.Errorf("insecure = %v, want %v", insecure, tc.wantInsecure)
			}
		})
	}
}
