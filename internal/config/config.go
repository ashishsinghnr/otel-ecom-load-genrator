// Package config defines the topology schema and loads and validates it.
//
// Validation runs before any exporter is constructed so that an invalid
// topology is a startup error rather than silently-missing telemetry.
package config

// File is the root of a topology JSON document.
type File struct {
	Topology   Topology    `json:"topology"`
	RootRoutes []RootRoute `json:"rootRoutes"`
	Surges     []Surge     `json:"surges"`
}

// Topology holds the simulated services.
type Topology struct {
	Services []Service `json:"services"`
}

// Service is one simulated service, exported as a distinct service.name.
type Service struct {
	ServiceName   string         `json:"serviceName"`
	Instances     []string       `json:"instances"`
	Tier          string         `json:"tier"`
	AttributeSets []AttributeSet `json:"attributeSets"`
	EventSets     []EventSet     `json:"eventSets"`
	Routes        []Route        `json:"routes"`
}

// Route is one operation on a service, emitted as a span.
type Route struct {
	Route           string            `json:"route"`
	SpanKind        string            `json:"spanKind"`
	DownstreamCalls map[string]string `json:"downstreamCalls"`
	Latency         Latency           `json:"latency"`
	AttributeSets   []AttributeSet    `json:"attributeSets"`
	EventSets       []EventSet        `json:"eventSets"`
	Metrics         RouteMetrics      `json:"metrics"`
}

// Latency describes the duration distribution for a route, in milliseconds.
type Latency struct {
	P50               int     `json:"p50"`
	P99               int     `json:"p99"`
	OutlierRate       float64 `json:"outlierRate"`
	OutlierMultiplier float64 `json:"outlierMultiplier"`
}

// AttributeSet is one weighted alternative set of span attributes.
//
// The key "error" is a directive rather than a literal attribute: when a set
// carrying error:true is selected, the span status is set to Error and an
// ERROR log record is emitted. See spec requirement C10.
type AttributeSet struct {
	Weight     int                    `json:"weight"`
	Attributes map[string]interface{} `json:"attributes"`
}

// EventSet is one weighted alternative set of span events.
type EventSet struct {
	Weight int     `json:"weight"`
	Events []Event `json:"events"`
}

// Event is a single span event.
type Event struct {
	Name       string                 `json:"name"`
	Attributes map[string]interface{} `json:"attributes"`
}

// RouteMetrics declares business metrics recorded when a route is visited.
// RED metrics are emitted for every route and need no declaration.
type RouteMetrics struct {
	Business []BusinessMetric `json:"business"`
}

// BusinessMetric is one declared business instrument.
type BusinessMetric struct {
	Name string  `json:"name"`
	Kind string  `json:"kind"` // "counter" or "histogram"
	Unit string  `json:"unit"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
}

// RootRoute is a traffic entry point driven at a fixed rate.
type RootRoute struct {
	Service       string `json:"service"`
	Route         string `json:"route"`
	TracesPerHour int    `json:"tracesPerHour"`
}

// Surge periodically multiplies the rate of the listed routes.
type Surge struct {
	EveryMinutes    int      `json:"everyMinutes"`
	DurationSeconds int      `json:"durationSeconds"`
	Multiplier      float64  `json:"multiplier"`
	Routes          []string `json:"routes"`
}

// ErrorDirectiveKey marks an attribute set as producing a failed span.
const ErrorDirectiveKey = "error"

// HasErrorDirective reports whether this set marks the span as failed.
func (a AttributeSet) HasErrorDirective() bool {
	v, ok := a.Attributes[ErrorDirectiveKey]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// FindService returns the named service, or nil.
func (t Topology) FindService(name string) *Service {
	for i := range t.Services {
		if t.Services[i].ServiceName == name {
			return &t.Services[i]
		}
	}
	return nil
}

// FindRoute returns the named route on this service, or nil.
func (s *Service) FindRoute(route string) *Route {
	for i := range s.Routes {
		if s.Routes[i].Route == route {
			return &s.Routes[i]
		}
	}
	return nil
}
