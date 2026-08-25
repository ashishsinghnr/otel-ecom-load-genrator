package engine

import (
	"strings"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// httpMethods are the tokens recognized as a leading HTTP method in a route
// string such as "POST /api/checkout".
var httpMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
	"HEAD": true, "OPTIONS": true, "TRACE": true, "CONNECT": true,
}

// routeAttributes derives semantic-convention attributes from a route string.
//
// This matters for more than tidiness: backends name a transaction from a
// server span's attributes, not from the span name. Without http.route, New
// Relic reports the transaction as "unknown". Routes come in three shapes
// across the shipped topologies, so each is mapped to its own conventions:
//
//	"POST /api/checkout"     -> http.request.method, http.route
//	"HGETALL"                -> db.operation.name
//	"publish order.placed"   -> messaging.operation.name, messaging.destination.name
func routeAttributes(route, spanKind string) []attribute.KeyValue {
	route = strings.TrimSpace(route)
	if route == "" {
		return nil
	}

	if method, path, ok := splitHTTPRoute(route); ok {
		// http.route is set for every HTTP span, not only server spans: it is
		// what identifies the operation, and client spans benefit from it too.
		attrs := []attribute.KeyValue{
			semconv.HTTPRequestMethodKey.String(method),
			semconv.HTTPRouteKey.String(path),
		}
		if isEntryPoint(spanKind) {
			// url.path is the concrete path; with no path parameters in these
			// synthetic routes it equals the route template.
			attrs = append(attrs, semconv.URLPathKey.String(path))
		}
		return attrs
	}

	if op, dest, ok := splitMessagingRoute(route); ok {
		return []attribute.KeyValue{
			semconv.MessagingOperationNameKey.String(op),
			semconv.MessagingDestinationNameKey.String(dest),
		}
	}

	// Anything else is treated as a database or internal operation name.
	return []attribute.KeyValue{
		semconv.DBOperationNameKey.String(route),
	}
}

// splitHTTPRoute splits "POST /api/checkout" into its method and path.
func splitHTTPRoute(route string) (method, path string, ok bool) {
	first, rest, found := strings.Cut(route, " ")
	if !found {
		return "", "", false
	}
	upper := strings.ToUpper(first)
	if !httpMethods[upper] {
		return "", "", false
	}
	path = strings.TrimSpace(rest)
	if path == "" {
		return "", "", false
	}
	return upper, path, true
}

// splitMessagingRoute splits "publish order.placed" into operation and
// destination.
func splitMessagingRoute(route string) (op, dest string, ok bool) {
	first, rest, found := strings.Cut(route, " ")
	if !found {
		return "", "", false
	}
	switch strings.ToLower(first) {
	case "publish", "send", "consume", "receive", "process":
		dest = strings.TrimSpace(rest)
		if dest == "" {
			return "", "", false
		}
		return strings.ToLower(first), dest, true
	default:
		return "", "", false
	}
}

// isEntryPoint reports whether a span kind makes the span a transaction entry
// point. Backends treat server and consumer spans this way.
func isEntryPoint(spanKind string) bool {
	switch strings.ToLower(spanKind) {
	case "server", "consumer":
		return true
	default:
		return false
	}
}
