package engine

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/telemetry"
)

func attrMap(kvs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func TestRouteAttributes_HTTP(t *testing.T) {
	got := attrMap(routeAttributes("POST /api/checkout", "server"))

	if got["http.request.method"] != "POST" {
		t.Errorf("http.request.method = %q, want POST", got["http.request.method"])
	}
	// http.route is what a backend names the transaction from.
	if got["http.route"] != "/api/checkout" {
		t.Errorf("http.route = %q, want /api/checkout", got["http.route"])
	}
	if got["url.path"] != "/api/checkout" {
		t.Errorf("url.path = %q, want /api/checkout for a server span", got["url.path"])
	}
}

func TestRouteAttributes_HTTPClientSpanOmitsURLPath(t *testing.T) {
	got := attrMap(routeAttributes("GET /cart", "client"))

	if got["http.route"] != "/cart" {
		t.Errorf("http.route = %q, want /cart", got["http.route"])
	}
	if _, ok := got["url.path"]; ok {
		t.Error("url.path should only be set on entry-point spans")
	}
}

func TestRouteAttributes_LowercaseMethodIsNormalized(t *testing.T) {
	got := attrMap(routeAttributes("post /x", "server"))
	if got["http.request.method"] != "POST" {
		t.Errorf("http.request.method = %q, want POST", got["http.request.method"])
	}
}

func TestRouteAttributes_Database(t *testing.T) {
	for _, route := range []string{"HGETALL", "SETNX", "SELECT products"} {
		got := attrMap(routeAttributes(route, "client"))
		if got["db.operation.name"] != route {
			t.Errorf("route %q: db.operation.name = %q, want %q", route, got["db.operation.name"], route)
		}
		if _, ok := got["http.route"]; ok {
			t.Errorf("route %q should not produce http.route", route)
		}
	}
}

func TestRouteAttributes_Messaging(t *testing.T) {
	got := attrMap(routeAttributes("publish order.placed", "producer"))
	if got["messaging.operation.name"] != "publish" {
		t.Errorf("messaging.operation.name = %q, want publish", got["messaging.operation.name"])
	}
	if got["messaging.destination.name"] != "order.placed" {
		t.Errorf("messaging.destination.name = %q, want order.placed", got["messaging.destination.name"])
	}

	got = attrMap(routeAttributes("consume order.placed", "consumer"))
	if got["messaging.operation.name"] != "consume" {
		t.Errorf("messaging.operation.name = %q, want consume", got["messaging.operation.name"])
	}
}

func TestRouteAttributes_EdgeCases(t *testing.T) {
	if got := routeAttributes("", "server"); got != nil {
		t.Errorf("empty route -> %v, want nil", got)
	}
	// "SELECT products" has a space but no HTTP method, so it is a db op.
	got := attrMap(routeAttributes("SELECT products", "client"))
	if got["db.operation.name"] != "SELECT products" {
		t.Errorf("db.operation.name = %q", got["db.operation.name"])
	}
	// A method with no path is not a valid HTTP route.
	got = attrMap(routeAttributes("GET", "server"))
	if got["db.operation.name"] != "GET" {
		t.Errorf("bare method should fall through to db op, got %v", got)
	}
}

// The end-to-end guarantee: every server span carries http.route, which is
// what stops a backend from reporting the transaction as "unknown".
func TestEmitTrace_ServerSpansCarryHTTPRoute(t *testing.T) {
	topo := config.Topology{Services: []config.Service{
		{
			ServiceName: "gateway",
			Instances:   []string{"gw-1"},
			Routes: []config.Route{{
				Route:           "POST /api/checkout",
				SpanKind:        "server",
				DownstreamCalls: map[string]string{"cache": "HGETALL"},
				Latency:         config.Latency{P50: 5, P99: 10, OutlierMultiplier: 1},
			}},
		},
		{
			ServiceName: "cache",
			Instances:   []string{"c-1"},
			Routes: []config.Route{{
				Route:    "HGETALL",
				SpanKind: "client",
				Latency:  config.Latency{P50: 1, P99: 2, OutlierMultiplier: 1},
			}},
		},
	}}

	h := telemetry.NewTestHarness(t, topo)
	e := New(topo, h.Providers, Options{})
	e.EmitTrace(context.Background(), "gateway", "POST /api/checkout")

	server := stubsByName(t, h, "POST /api/checkout")
	got := attrMap(server.Attributes())

	if got["http.route"] != "/api/checkout" {
		t.Errorf("server span http.route = %q, want /api/checkout; without it the backend reports the transaction as unknown", got["http.route"])
	}
	if got["http.request.method"] != "POST" {
		t.Errorf("server span http.request.method = %q, want POST", got["http.request.method"])
	}

	// The db span gets db conventions rather than HTTP ones.
	dbSpan := stubsByName(t, h, "HGETALL")
	dbAttrs := attrMap(dbSpan.Attributes())
	if dbAttrs["db.operation.name"] != "HGETALL" {
		t.Errorf("db span db.operation.name = %q, want HGETALL", dbAttrs["db.operation.name"])
	}
}

// Every server or consumer span in the shipped topologies must be nameable,
// or some transactions will show as unknown even when others work.
func TestShippedTopologies_EntryPointsAreNameable(t *testing.T) {
	for _, path := range []string{
		"../../topologies/shop-smoke.json",
		"../../topologies/shop-full.json",
	} {
		f, _, err := config.Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}

		for _, svc := range f.Topology.Services {
			for _, r := range svc.Routes {
				if !isEntryPoint(r.SpanKind) {
					continue
				}
				attrs := attrMap(routeAttributes(r.Route, r.SpanKind))
				_, hasHTTP := attrs["http.route"]
				_, hasMsg := attrs["messaging.destination.name"]
				if !hasHTTP && !hasMsg {
					t.Errorf("%s: %s route %q is an entry point (%s) but produces no http.route or messaging.destination.name, so its transaction would be unnamed; attrs=%v",
						path, svc.ServiceName, r.Route, r.SpanKind, attrs)
				}
			}
		}
	}
}
