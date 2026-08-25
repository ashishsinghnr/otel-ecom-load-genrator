package e2e

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// spanFacts is what a backend needs in order to name a transaction.
type spanFacts struct {
	name       string
	kind       string
	service    string
	attributes map[string]string
}

// collectSpanFacts runs the generator and returns every exported span with its
// attributes decoded from the real protobuf.
func collectSpanFacts(t *testing.T, topology string, extra ...string) []spanFacts {
	t.Helper()

	var (
		mu    sync.Mutex
		facts []spanFacts
	)

	sink, endpoint := startSinkWithHook(t, func(rs *tracepb.ResourceSpans) {
		service := attrString(rs.Resource.GetAttributes(), "service.name")
		mu.Lock()
		defer mu.Unlock()
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				facts = append(facts, spanFacts{
					name:       sp.Name,
					kind:       sp.Kind.String(),
					service:    service,
					attributes: kvMap(sp.Attributes),
				})
			}
		}
	})
	_ = sink

	bin := buildBinary(t)
	runGenerator(t, bin, topology, endpoint, extra...)
	time.Sleep(700 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	out := make([]spanFacts, len(facts))
	copy(out, facts)
	return out
}

func kvMap(kvs []*commonpb.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[kv.Key] = kv.Value.GetStringValue()
		if s := kv.Value.GetStringValue(); s == "" {
			// Non-string values still matter for presence checks.
			switch v := kv.Value.Value.(type) {
			case *commonpb.AnyValue_IntValue:
				out[kv.Key] = itoa(v.IntValue)
			case *commonpb.AnyValue_BoolValue:
				if v.BoolValue {
					out[kv.Key] = "true"
				} else {
					out[kv.Key] = "false"
				}
			}
		}
	}
	return out
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}

// Entry-point spans must carry the attributes a backend names a transaction
// from. Without http.route, New Relic reports the transaction as "unknown".
func TestEndToEnd_EntryPointSpansAreNameable(t *testing.T) {
	facts := collectSpanFacts(t, "../../topologies/shop-smoke.json")
	if len(facts) == 0 {
		t.Fatal("no spans exported")
	}

	entryPoints := 0
	for _, f := range facts {
		if f.kind != "SPAN_KIND_SERVER" && f.kind != "SPAN_KIND_CONSUMER" {
			continue
		}
		entryPoints++

		route := f.attributes["http.route"]
		dest := f.attributes["messaging.destination.name"]
		if route == "" && dest == "" {
			t.Errorf("entry-point span %q on %s has neither http.route nor messaging.destination.name; its transaction would show as unknown (attrs=%v)",
				f.name, f.service, f.attributes)
			continue
		}
		if route != "" && !strings.HasPrefix(route, "/") {
			t.Errorf("span %q: http.route = %q, want a leading slash", f.name, route)
		}
	}

	if entryPoints == 0 {
		t.Fatal("no server or consumer spans were exported; nothing would appear as a transaction")
	}
	t.Logf("verified %d entry-point spans out of %d total", entryPoints, len(facts))
}

// HTTP spans must carry the method, and database spans the operation, so each
// span type is classified correctly by the backend.
func TestEndToEnd_SpanConventionsByType(t *testing.T) {
	facts := collectSpanFacts(t, "../../topologies/shop-smoke.json")

	var httpSpans, dbSpans int
	for _, f := range facts {
		switch {
		case f.attributes["http.route"] != "":
			httpSpans++
			if f.attributes["http.request.method"] == "" {
				t.Errorf("span %q has http.route but no http.request.method", f.name)
			}
		case f.attributes["db.operation.name"] != "":
			dbSpans++
		}
	}

	if httpSpans == 0 {
		t.Error("no HTTP spans exported")
	}
	if dbSpans == 0 {
		t.Error("no database spans exported")
	}
	t.Logf("http spans=%d db spans=%d", httpSpans, dbSpans)
}
