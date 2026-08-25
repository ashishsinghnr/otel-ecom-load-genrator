package config

import (
	"strings"
	"testing"
)

// validTopology returns a minimal topology that passes validation.
// Tests mutate a copy of it to isolate a single failure.
func validTopology() *File {
	return &File{
		Topology: Topology{Services: []Service{
			{
				ServiceName: "web",
				Instances:   []string{"web-1"},
				Routes: []Route{{
					Route:           "GET /",
					SpanKind:        "server",
					DownstreamCalls: map[string]string{"cart": "GET /cart"},
					Latency:         Latency{P50: 10, P99: 100, OutlierRate: 0.01, OutlierMultiplier: 5},
				}},
			},
			{
				ServiceName: "cart",
				Instances:   []string{"cart-1"},
				Routes: []Route{{
					Route:    "GET /cart",
					SpanKind: "server",
					Latency:  Latency{P50: 5, P99: 20, OutlierMultiplier: 1},
				}},
			},
		}},
		RootRoutes: []RootRoute{{Service: "web", Route: "GET /", TracesPerHour: 100}},
	}
}

func TestValidate_AcceptsValidTopology(t *testing.T) {
	if err := Validate(validTopology()); err != nil {
		t.Fatalf("expected valid topology to pass, got: %v", err)
	}
}

// C3: every downstream reference must resolve.
func TestValidate_RejectsDanglingReferences(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*File)
		wantSub string
	}{
		{
			name: "unknown downstream service",
			mutate: func(f *File) {
				f.Topology.Services[0].Routes[0].DownstreamCalls = map[string]string{"ghost": "GET /x"}
			},
			wantSub: "unknown service",
		},
		{
			name: "unknown downstream route",
			mutate: func(f *File) {
				f.Topology.Services[0].Routes[0].DownstreamCalls = map[string]string{"cart": "GET /nope"}
			},
			wantSub: "unknown route",
		},
		{
			name: "root route unknown service",
			mutate: func(f *File) {
				f.RootRoutes[0].Service = "ghost"
			},
			wantSub: "unknown service",
		},
		{
			name: "root route unknown route",
			mutate: func(f *File) {
				f.RootRoutes[0].Route = "GET /nope"
			},
			wantSub: "unknown route",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := validTopology()
			tc.mutate(f)
			err := Validate(f)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// C4: the call graph must be acyclic.
func TestValidate_RejectsCycles(t *testing.T) {
	t.Run("two node cycle", func(t *testing.T) {
		f := validTopology()
		// cart calls back into web
		f.Topology.Services[1].Routes[0].DownstreamCalls = map[string]string{"web": "GET /"}
		err := Validate(f)
		if err == nil {
			t.Fatal("expected cycle to be rejected")
		}
		if !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("error %q does not mention cycle", err.Error())
		}
	})

	t.Run("self route cycle", func(t *testing.T) {
		f := validTopology()
		f.Topology.Services[1].Routes[0].DownstreamCalls = map[string]string{"cart": "GET /cart"}
		err := Validate(f)
		if err == nil {
			t.Fatal("expected self-cycle to be rejected")
		}
	})
}

// Self-calls to a *different* route are legal and must not be flagged.
func TestValidate_AllowsSelfCallToDifferentRoute(t *testing.T) {
	f := validTopology()
	cart := &f.Topology.Services[1]
	cart.Routes = append(cart.Routes, Route{
		Route:    "GET /cart/items",
		SpanKind: "internal",
		Latency:  Latency{P50: 2, P99: 8, OutlierMultiplier: 1},
	})
	cart.Routes[0].DownstreamCalls = map[string]string{"cart": "GET /cart/items"}

	if err := Validate(f); err != nil {
		t.Fatalf("self-call to a different route should be legal, got: %v", err)
	}
}

// C7: each attribute/event set list needs at least one positive weight.
func TestValidate_RejectsAllZeroWeights(t *testing.T) {
	t.Run("attribute sets", func(t *testing.T) {
		f := validTopology()
		f.Topology.Services[0].AttributeSets = []AttributeSet{
			{Attributes: map[string]interface{}{"a": "b"}}, // weight omitted -> 0
		}
		err := Validate(f)
		if err == nil {
			t.Fatal("expected all-zero attribute weights to be rejected")
		}
		if !strings.Contains(err.Error(), "weight") {
			t.Fatalf("error %q does not mention weight", err.Error())
		}
	})

	t.Run("event sets", func(t *testing.T) {
		f := validTopology()
		f.Topology.Services[0].EventSets = []EventSet{
			{Events: []Event{{Name: "e"}}},
		}
		if err := Validate(f); err == nil {
			t.Fatal("expected all-zero event weights to be rejected")
		}
	})

	t.Run("negative weight", func(t *testing.T) {
		f := validTopology()
		f.Topology.Services[0].AttributeSets = []AttributeSet{{Weight: -5}}
		if err := Validate(f); err == nil {
			t.Fatal("expected negative weight to be rejected")
		}
	})

	t.Run("empty list is fine", func(t *testing.T) {
		f := validTopology()
		f.Topology.Services[0].AttributeSets = nil
		if err := Validate(f); err != nil {
			t.Fatalf("empty attribute set list should be legal, got: %v", err)
		}
	})
}

// Latency bounds guard the sampler against impossible ranges (C1).
func TestValidate_RejectsBadLatency(t *testing.T) {
	tests := []struct {
		name string
		lat  Latency
	}{
		{"p50 zero", Latency{P50: 0, P99: 10, OutlierMultiplier: 1}},
		{"p50 negative", Latency{P50: -1, P99: 10, OutlierMultiplier: 1}},
		{"p99 below p50", Latency{P50: 100, P99: 10, OutlierMultiplier: 1}},
		{"outlier rate 1", Latency{P50: 1, P99: 10, OutlierRate: 1, OutlierMultiplier: 1}},
		{"outlier rate negative", Latency{P50: 1, P99: 10, OutlierRate: -0.5, OutlierMultiplier: 1}},
		{"outlier multiplier below 1", Latency{P50: 1, P99: 10, OutlierRate: 0.1, OutlierMultiplier: 0.5}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := validTopology()
			f.Topology.Services[1].Routes[0].Latency = tc.lat
			if err := Validate(f); err == nil {
				t.Fatalf("expected latency %+v to be rejected", tc.lat)
			}
		})
	}
}

// p50 == p99 is a legal degenerate case: fixed duration, no jitter.
func TestValidate_AllowsEqualPercentiles(t *testing.T) {
	f := validTopology()
	f.Topology.Services[1].Routes[0].Latency = Latency{P50: 50, P99: 50, OutlierMultiplier: 1}
	if err := Validate(f); err != nil {
		t.Fatalf("p50 == p99 should be legal, got: %v", err)
	}
}

func TestValidate_RejectsStructuralProblems(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*File)
		wantSub string
	}{
		{
			name:    "no services",
			mutate:  func(f *File) { f.Topology.Services = nil },
			wantSub: "at least one service",
		},
		{
			name:    "no root routes",
			mutate:  func(f *File) { f.RootRoutes = nil },
			wantSub: "at least one root route",
		},
		{
			name:    "duplicate service name",
			mutate:  func(f *File) { f.Topology.Services[1].ServiceName = "web" },
			wantSub: "duplicate",
		},
		{
			name: "duplicate route on one service",
			mutate: func(f *File) {
				s := &f.Topology.Services[1]
				s.Routes = append(s.Routes, s.Routes[0])
			},
			wantSub: "duplicate",
		},
		{
			name:    "empty service name",
			mutate:  func(f *File) { f.Topology.Services[0].ServiceName = "" },
			wantSub: "serviceName",
		},
		{
			name:    "service with no routes",
			mutate:  func(f *File) { f.Topology.Services[1].Routes = nil },
			wantSub: "no routes",
		},
		{
			name:    "no instances",
			mutate:  func(f *File) { f.Topology.Services[0].Instances = nil },
			wantSub: "instance",
		},
		{
			name:    "zero tracesPerHour",
			mutate:  func(f *File) { f.RootRoutes[0].TracesPerHour = 0 },
			wantSub: "tracesPerHour",
		},
		{
			name:    "bad span kind",
			mutate:  func(f *File) { f.Topology.Services[0].Routes[0].SpanKind = "sideways" },
			wantSub: "spanKind",
		},
		{
			name: "bad metric kind",
			mutate: func(f *File) {
				f.Topology.Services[0].Routes[0].Metrics.Business = []BusinessMetric{
					{Name: "x", Kind: "gauge-ish"},
				}
			},
			wantSub: "kind",
		},
		{
			name: "histogram with max below min",
			mutate: func(f *File) {
				f.Topology.Services[0].Routes[0].Metrics.Business = []BusinessMetric{
					{Name: "x", Kind: "histogram", Min: 10, Max: 1},
				}
			},
			wantSub: "max",
		},
		{
			name: "surge bad multiplier",
			mutate: func(f *File) {
				f.Surges = []Surge{{EveryMinutes: 5, DurationSeconds: 10, Multiplier: 0.5}}
			},
			wantSub: "multiplier",
		},
		{
			name: "surge duration exceeds period",
			mutate: func(f *File) {
				f.Surges = []Surge{{EveryMinutes: 1, DurationSeconds: 120, Multiplier: 2}}
			},
			wantSub: "duration",
		},
		{
			name: "surge unknown route",
			mutate: func(f *File) {
				f.Surges = []Surge{{EveryMinutes: 5, DurationSeconds: 10, Multiplier: 2, Routes: []string{"GET /ghost"}}}
			},
			wantSub: "unknown route",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := validTopology()
			tc.mutate(f)
			err := Validate(f)
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.wantSub)) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// C11: weights that do not sum to 100 are reported as a warning, not an error.
func TestValidate_WarnsOnWeightSum(t *testing.T) {
	f := validTopology()
	f.Topology.Services[0].AttributeSets = []AttributeSet{
		{Weight: 25, Attributes: map[string]interface{}{"error": true}},
		{Weight: 85, Attributes: map[string]interface{}{}},
	}

	warnings, err := ValidateWithWarnings(f)
	if err != nil {
		t.Fatalf("weights not summing to 100 must not be fatal, got: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "110") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning naming the actual sum 110, got: %v", warnings)
	}
}

// A service unreachable from any root route is a warning, not an error.
func TestValidate_WarnsOnUnreachableService(t *testing.T) {
	f := validTopology()
	f.Topology.Services = append(f.Topology.Services, Service{
		ServiceName: "orphan",
		Instances:   []string{"orphan-1"},
		Routes:      []Route{{Route: "GET /x", Latency: Latency{P50: 1, P99: 2, OutlierMultiplier: 1}}},
	})

	warnings, err := ValidateWithWarnings(f)
	if err != nil {
		t.Fatalf("unreachable service must not be fatal, got: %v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "orphan") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning naming the unreachable service, got: %v", warnings)
	}
}
