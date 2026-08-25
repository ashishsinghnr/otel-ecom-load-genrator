package config

import (
	"errors"
	"fmt"
	"sort"
)

// validSpanKinds are the span kinds accepted in a route's spanKind field.
// An empty value is allowed and means "internal".
var validSpanKinds = map[string]bool{
	"": true, "internal": true, "server": true,
	"client": true, "producer": true, "consumer": true,
}

// Validate reports the first structural problem with f, or nil.
// Warnings are discarded; use ValidateWithWarnings to receive them.
func Validate(f *File) error {
	_, err := ValidateWithWarnings(f)
	return err
}

// ValidateWithWarnings validates f, returning non-fatal warnings alongside
// any fatal error. Every fatal error names the offending path in the document.
func ValidateWithWarnings(f *File) ([]string, error) {
	if f == nil {
		return nil, errors.New("topology is nil")
	}

	var warnings []string

	if len(f.Topology.Services) == 0 {
		return warnings, errors.New("topology must declare at least one service")
	}
	if len(f.RootRoutes) == 0 {
		return warnings, errors.New("topology must declare at least one root route")
	}

	// Per-service structure, and a uniqueness index for reference checks.
	seenService := map[string]bool{}
	for i := range f.Topology.Services {
		s := &f.Topology.Services[i]
		path := fmt.Sprintf("services[%d]", i)

		if s.ServiceName == "" {
			return warnings, fmt.Errorf("%s: serviceName must not be empty", path)
		}
		path = fmt.Sprintf("services[%s]", s.ServiceName)

		if seenService[s.ServiceName] {
			return warnings, fmt.Errorf("%s: duplicate serviceName", path)
		}
		seenService[s.ServiceName] = true

		if len(s.Instances) == 0 {
			return warnings, fmt.Errorf("%s: must declare at least one instance", path)
		}
		for j, inst := range s.Instances {
			if inst == "" {
				return warnings, fmt.Errorf("%s.instances[%d]: instance must not be empty", path, j)
			}
		}
		if len(s.Routes) == 0 {
			return warnings, fmt.Errorf("%s: has no routes", path)
		}

		w, err := validateWeighted(path, s.AttributeSets, s.EventSets)
		warnings = append(warnings, w...)
		if err != nil {
			return warnings, err
		}

		seenRoute := map[string]bool{}
		for j := range s.Routes {
			r := &s.Routes[j]
			rp := fmt.Sprintf("%s.routes[%d]", path, j)
			if r.Route == "" {
				return warnings, fmt.Errorf("%s: route must not be empty", rp)
			}
			rp = fmt.Sprintf("%s.routes[%s]", path, r.Route)

			if seenRoute[r.Route] {
				return warnings, fmt.Errorf("%s: duplicate route", rp)
			}
			seenRoute[r.Route] = true

			if !validSpanKinds[r.SpanKind] {
				return warnings, fmt.Errorf("%s: invalid spanKind %q (want internal, server, client, producer or consumer)", rp, r.SpanKind)
			}
			if err := validateLatency(rp, r.Latency); err != nil {
				return warnings, err
			}
			w, err := validateWeighted(rp, r.AttributeSets, r.EventSets)
			warnings = append(warnings, w...)
			if err != nil {
				return warnings, err
			}
			for k, bm := range r.Metrics.Business {
				if err := validateBusinessMetric(fmt.Sprintf("%s.metrics.business[%d]", rp, k), bm); err != nil {
					return warnings, err
				}
			}
		}
	}

	// Reference resolution (C3). Done in a second pass so forward references work.
	for i := range f.Topology.Services {
		s := &f.Topology.Services[i]
		for j := range s.Routes {
			r := &s.Routes[j]
			rp := fmt.Sprintf("services[%s].routes[%s]", s.ServiceName, r.Route)
			for dSvc, dRoute := range r.DownstreamCalls {
				target := f.Topology.FindService(dSvc)
				if target == nil {
					return warnings, fmt.Errorf("%s.downstreamCalls: unknown service %q", rp, dSvc)
				}
				if target.FindRoute(dRoute) == nil {
					return warnings, fmt.Errorf("%s.downstreamCalls: unknown route %q on service %q", rp, dRoute, dSvc)
				}
			}
		}
	}

	for i, rr := range f.RootRoutes {
		rp := fmt.Sprintf("rootRoutes[%d]", i)
		svc := f.Topology.FindService(rr.Service)
		if svc == nil {
			return warnings, fmt.Errorf("%s: unknown service %q", rp, rr.Service)
		}
		if svc.FindRoute(rr.Route) == nil {
			return warnings, fmt.Errorf("%s: unknown route %q on service %q", rp, rr.Route, rr.Service)
		}
		if rr.TracesPerHour <= 0 {
			return warnings, fmt.Errorf("%s: tracesPerHour must be greater than 0", rp)
		}
	}

	// Cycle detection (C4) over (service, route) nodes.
	if err := detectCycles(f); err != nil {
		return warnings, err
	}

	for i, s := range f.Surges {
		if err := validateSurge(fmt.Sprintf("surges[%d]", i), s, f); err != nil {
			return warnings, err
		}
	}

	warnings = append(warnings, unreachableServices(f)...)
	return warnings, nil
}

// validateWeighted enforces C7: a non-empty weighted list must contain at
// least one positive weight, and no negative weights. It warns when weights
// do not sum to 100 (C11), since that usually means the author read them
// as percentages.
func validateWeighted(path string, attrs []AttributeSet, events []EventSet) ([]string, error) {
	var warnings []string

	if len(attrs) > 0 {
		sum := 0
		for i, a := range attrs {
			if a.Weight < 0 {
				return warnings, fmt.Errorf("%s.attributeSets[%d]: weight must not be negative", path, i)
			}
			sum += a.Weight
		}
		if sum == 0 {
			return warnings, fmt.Errorf("%s.attributeSets: needs at least one positive weight (an omitted weight is 0, making the set unselectable)", path)
		}
		if sum != 100 {
			warnings = append(warnings, fmt.Sprintf("%s.attributeSets: weights sum to %d, not 100; they are normalized, so shares are relative to %d", path, sum, sum))
		}
	}

	if len(events) > 0 {
		sum := 0
		for i, e := range events {
			if e.Weight < 0 {
				return warnings, fmt.Errorf("%s.eventSets[%d]: weight must not be negative", path, i)
			}
			sum += e.Weight
		}
		if sum == 0 {
			return warnings, fmt.Errorf("%s.eventSets: needs at least one positive weight (an omitted weight is 0, making the set unselectable)", path)
		}
	}

	return warnings, nil
}

// validateLatency guards the sampler against ranges it cannot sample (C1).
func validateLatency(path string, l Latency) error {
	if l.P50 < 1 {
		return fmt.Errorf("%s.latency.p50: must be at least 1ms, got %d", path, l.P50)
	}
	if l.P99 < l.P50 {
		return fmt.Errorf("%s.latency.p99: must be greater than or equal to p50 (%d), got %d", path, l.P50, l.P99)
	}
	if l.OutlierRate < 0 || l.OutlierRate >= 1 {
		return fmt.Errorf("%s.latency.outlierRate: must be in [0, 1), got %v", path, l.OutlierRate)
	}
	if l.OutlierRate > 0 && l.OutlierMultiplier < 1 {
		return fmt.Errorf("%s.latency.outlierMultiplier: must be at least 1, got %v", path, l.OutlierMultiplier)
	}
	return nil
}

func validateBusinessMetric(path string, m BusinessMetric) error {
	if m.Name == "" {
		return fmt.Errorf("%s: name must not be empty", path)
	}
	switch m.Kind {
	case "counter", "histogram":
	default:
		return fmt.Errorf("%s: invalid kind %q (want counter or histogram)", path, m.Kind)
	}
	if m.Kind == "histogram" && m.Max < m.Min {
		return fmt.Errorf("%s: max (%v) must be greater than or equal to min (%v)", path, m.Max, m.Min)
	}
	return nil
}

func validateSurge(path string, s Surge, f *File) error {
	if s.EveryMinutes <= 0 {
		return fmt.Errorf("%s.everyMinutes: must be greater than 0", path)
	}
	if s.DurationSeconds <= 0 {
		return fmt.Errorf("%s.durationSeconds: must be greater than 0", path)
	}
	if s.DurationSeconds >= s.EveryMinutes*60 {
		return fmt.Errorf("%s: duration (%ds) must be shorter than the period (%dm)", path, s.DurationSeconds, s.EveryMinutes)
	}
	if s.Multiplier < 1 {
		return fmt.Errorf("%s.multiplier: must be at least 1, got %v", path, s.Multiplier)
	}
	// An empty Routes list means "all root routes".
	for _, want := range s.Routes {
		found := false
		for _, rr := range f.RootRoutes {
			if rr.Route == want {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s.routes: unknown route %q (must name a root route)", path, want)
		}
	}
	return nil
}

// node identifies one (service, route) pair in the call graph.
type node struct {
	service string
	route   string
}

// detectCycles walks the call graph depth-first from every root route and
// reports the first cycle found. Without this, a cyclic topology recurses
// without bound at emission time (C4).
func detectCycles(f *File) error {
	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := map[node]int{}
	var stack []node

	var visit func(n node) error
	visit = func(n node) error {
		switch state[n] {
		case onStack:
			return fmt.Errorf("topology contains a cycle: %s", formatCycle(stack, n))
		case done:
			return nil
		}

		state[n] = onStack
		stack = append(stack, n)

		svc := f.Topology.FindService(n.service)
		if svc != nil {
			if r := svc.FindRoute(n.route); r != nil {
				// Sort for deterministic error messages across runs.
				keys := make([]string, 0, len(r.DownstreamCalls))
				for k := range r.DownstreamCalls {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, dSvc := range keys {
					if err := visit(node{dSvc, r.DownstreamCalls[dSvc]}); err != nil {
						return err
					}
				}
			}
		}

		stack = stack[:len(stack)-1]
		state[n] = done
		return nil
	}

	for _, rr := range f.RootRoutes {
		if err := visit(node{rr.Service, rr.Route}); err != nil {
			return err
		}
	}
	return nil
}

// formatCycle renders the path from the repeated node onward, e.g.
// "web:GET / -> cart:GET /cart -> web:GET /".
func formatCycle(stack []node, repeat node) string {
	start := 0
	for i, n := range stack {
		if n == repeat {
			start = i
			break
		}
	}
	out := ""
	for _, n := range stack[start:] {
		out += fmt.Sprintf("%s:%s -> ", n.service, n.route)
	}
	return out + fmt.Sprintf("%s:%s", repeat.service, repeat.route)
}

// unreachableServices warns about services no root route can reach, which
// usually means a typo or a leftover definition.
func unreachableServices(f *File) []string {
	reached := map[string]bool{}

	var visit func(n node)
	visited := map[node]bool{}
	visit = func(n node) {
		if visited[n] {
			return
		}
		visited[n] = true
		reached[n.service] = true

		svc := f.Topology.FindService(n.service)
		if svc == nil {
			return
		}
		r := svc.FindRoute(n.route)
		if r == nil {
			return
		}
		for dSvc, dRoute := range r.DownstreamCalls {
			visit(node{dSvc, dRoute})
		}
	}

	for _, rr := range f.RootRoutes {
		visit(node{rr.Service, rr.Route})
	}

	var out []string
	for _, s := range f.Topology.Services {
		if !reached[s.ServiceName] {
			out = append(out, fmt.Sprintf("service %q is not reachable from any root route and will emit nothing", s.ServiceName))
		}
	}
	return out
}
