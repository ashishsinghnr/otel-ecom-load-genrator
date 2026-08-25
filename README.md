# otel-ecom-load-genrator

A topology-driven synthetic OpenTelemetry load generator for an e-commerce
domain. Describe a fake shop in JSON; get continuous traces, span events,
metrics, and logs over OTLP.

Built to feed a real backend — New Relic by default — so you can build
dashboards, test alerts, and demo observability without deploying an actual
application.

## Quick start

```sh
# Verify a topology without sending anything
go run ./cmd/otel-ecom-load-genrator \
  --topology topologies/shop-smoke.json --validate-only

# Send to New Relic (US region)
export NEW_RELIC_LICENSE_KEY=your-ingest-key
go run ./cmd/otel-ecom-load-genrator --topology topologies/shop-full.json

# Send to a local collector instead
docker compose up -d
go run ./cmd/otel-ecom-load-genrator \
  --topology topologies/shop-smoke.json --backend local --duration 30s
# then open Jaeger at http://localhost:16686
```

## What it produces

From one process, per simulated service:

| Signal | Detail |
|---|---|
| **Traces** | One span per route visit, correct parent/child linkage, span kinds, weighted attributes |
| **Span events** | `payment.authorized`, `inventory.reserved`, `cart.item_added`, … |
| **Metrics — RED** | `http.server.request.duration` (exponential histogram), `.request.count`, `.error.count` |
| **Metrics — business** | `orders.placed`, `cart.value`, `payment.amount`, `fraud.checks`, … |
| **Logs** | Emitted inside span context, so `trace_id`/`span_id` attach and trace↔log pivoting works |

Each service gets its own `resource` (`service.name`, `service.namespace`,
`service.instance.id`, `service.tier`), which is what makes a single process
appear as an 18-service architecture in a service map.

## Shipped topologies

**`topologies/shop-full.json`** — 18 services across five tiers:

- **edge** — `ashish-api-gateway`, `ashish-authentication-service`
- **application** — `ashish-search-service`, `ashish-product-catalog-service`, `ashish-pricing-service`,
  `ashish-promotion-service`, `ashish-review-service`, `ashish-cart-service`, `ashish-checkout-service`,
  `ashish-fraud-detection-service`, `ashish-payment-service`
- **fulfillment** — `ashish-inventory-service`, `ashish-order-management-service`, `ashish-shipping-service`
- **async** — `ashish-order-events-consumer` (producer → consumer hop), `ashish-notification-service`
- **data** — `ashish-postgres-db`, `ashish-redis-cache`

Six root routes covering home, search, product, add-to-cart, checkout, and
order-status, at 2,400–18,000 traces/hour each.

**`topologies/shop-smoke.json`** — 3 services, and the one to reach for
day to day. Small enough to hold in your head, but it still exercises every
feature: two root routes (a browse flow and a checkout flow), fan-out, a
same-service internal call, error injection on three routes, latency
outliers, and a surge window every 5 minutes.

```
ashish-api-gateway  GET  /api/cart      -> ashish-cart-service GET /cart
                                             └─ ashish-redis-cache HGETALL

ashish-api-gateway  POST /api/checkout  -> ashish-cart-service POST /checkout
                                             ├─ ashish-redis-cache HGETALL
                                             └─ ashish-cart-service POST /reserve-stock
                                                  └─ ashish-redis-cache SETNX
```

A 3-second run produces roughly 29 spans across 7 traces, with logs and
7 metric streams.

## CLI

```
--topology string      Path to the topology JSON file (required)
--backend string       newrelic | otlp | local (default "newrelic")
--endpoint string      OTLP endpoint URL or host:port (overrides everything below)
--protocol string      grpc (port 4317) | http (port 4318) (default "grpc")
--nr-region string     us | eu (default "us")
--namespace string     service.namespace (default "ecom")
--environment string   deployment.environment.name (default "synthetic")
--timing string        synthetic | nested (default "synthetic")
--duration duration    Stop after this long (0 = until interrupted)
--report-every dur     Log a progress line at this interval (default 30s, 0 disables)
--validate-only        Validate the topology and exit
--pprof string         pprof listen address, e.g. localhost:6060
--log-level string     debug | info | warn | error (default "info")
```

Environment:

- `NEW_RELIC_LICENSE_KEY` — required with `--backend=newrelic`. Read from the
  environment only, never a flag, since flags leak into shell history and
  process listings.
- `OTEL_EXPORTER_OTLP_*` — standard OTLP variables. An explicit endpoint
  overrides the `--backend` default, so the tool works with any OTLP consumer.

### New Relic specifics

Three settings are applied automatically, because they are easy to get
silently wrong:

1. **`api-key` header** from `NEW_RELIC_LICENSE_KEY`.
2. **Delta temporality.** New Relic requires it; the SDK defaults to
   cumulative, which is accepted but produces wrong values.
3. **Exponential histograms**, which New Relic prefers for long-tailed
   distributions like request duration.

Endpoints:

| Backend and region | `--protocol grpc` | `--protocol http` |
|---|---|---|
| `newrelic` + `us` (default) | `https://staging-otlp.nr-data.net:4317` | `https://staging-otlp.nr-data.net:4318` |
| `newrelic` + `eu` | `https://otlp.eu01.nr-data.net:4317` | `https://otlp.eu01.nr-data.net:4318` |
| `local` | `http://localhost:4317` | `http://localhost:4318` |
| `otlp` | from `OTEL_EXPORTER_OTLP_ENDPOINT` | same |

The protocol picks the port, so `--protocol http` reaches 4318 without you
specifying a URL. gRPC traffic sent to 4318 fails, which is the usual cause of
"it works with curl but not with the SDK".

**Setting the endpoint.** The table above lists defaults, not constraints.
Three ways to override, highest precedence first:

```sh
# 1. --endpoint flag: wins over everything
--endpoint https://my-collector.internal:4318
--endpoint my-collector.internal:4318          # bare host:port also works

# 2. the standard OTLP environment variable
export OTEL_EXPORTER_OTLP_ENDPOINT=https://my-collector.internal:4318

# 3. otherwise, the --backend default for the chosen protocol
```

The startup log states which source won, so a run is never ambiguous:

```
msg="starting load generator" endpoint="http://127.0.0.1:4318 (--endpoint)" protocol=http
```

The US host is New Relic **staging**, on purpose: this is synthetic test data,
and pointing it at a production account would pollute real dashboards and
consume real ingest. To target production, change `newRelicUSHost` in
[internal/telemetry/backend.go](internal/telemetry/backend.go).

## Topology schema

```json
{
  "topology": {
    "services": [{
      "serviceName": "ashish-checkout-service",
      "tier": "application",
      "instances": ["ashish-checkout-service-1a2b-ffff", "ashish-checkout-service-1a2b-0000"],
      "attributeSets": [{ "weight": 100, "attributes": { "version": "v6.0.1" } }],
      "eventSets":     [{ "weight": 100, "events": [{ "name": "order.created" }] }],
      "routes": [{
        "route": "POST /place-order",
        "spanKind": "server",
        "downstreamCalls": { "ashish-payment-service": "POST /authorize" },
        "latency": { "p50": 320, "p99": 1500, "outlierRate": 0.05, "outlierMultiplier": 8 },
        "attributeSets": [
          { "weight": 90, "attributes": { "http.response.status_code": 200 } },
          { "weight": 10, "attributes": { "error": true, "http.response.status_code": 503 } }
        ],
        "metrics": {
          "business": [
            { "name": "orders.placed", "kind": "counter", "unit": "{order}" },
            { "name": "order.value", "kind": "histogram", "unit": "USD", "min": 12, "max": 900 }
          ]
        }
      }]
    }]
  },
  "rootRoutes": [
    { "service": "ashish-api-gateway", "route": "POST /api/checkout", "tracesPerHour": 2400 }
  ],
  "surges": [
    { "everyMinutes": 15, "durationSeconds": 120, "multiplier": 6,
      "routes": ["POST /api/checkout"] }
  ]
}
```

### Notes on the schema

- **`latency`** is a distribution, not a ceiling. `p50` is a true median: half
  the samples fall at or below it, the rest are drawn log-uniformly up to
  `p99`, and `outlierRate` adds a tail beyond it. That produces a realistic
  right-skewed histogram rather than a flat one.
- **`error: true`** is a *directive*, not an exported attribute. When its set
  is selected, the span status becomes `Error`, `error.type` is set, and an
  ERROR log record is emitted. This is what makes backends count the span as
  a failure.
- **`weight`** must be positive. Weights are normalized against their actual
  sum, so they need not total 100 — but a sum other than 100 produces a
  warning, since it usually means someone read them as percentages.
- **`surges`** multiply a route's rate for `durationSeconds` every
  `everyMinutes`. Overlapping surges compound.

### Validation

The topology is fully validated before any exporter is built, so a mistake is
a startup error rather than silently-missing telemetry. Checked: every
downstream service and route reference resolves, the call graph is acyclic,
weights are positive, latency bounds are sane, names are unique, and surges
name real root routes. Unreachable services and odd weight sums produce
warnings.

## Timing

The default `--timing synthetic` computes span end times without sleeping,
which keeps throughput high.

**Known consequence:** children are emitted concurrently with independently
sampled durations, so a parent span can end before its children. Waterfall
views will sometimes show children extending past their parent, and
critical-path analysis is not trustworthy on this data.

All end-time computation lives in one place, so `--timing nested` (sleep so
durations match wall clock) is a small change; it is declared but not
implemented in this version.

## Development

```sh
make test        # unit + integration tests
make test-race   # with the race detector
make e2e         # compile the binary and run it against an in-process OTLP server
make lint        # go vet + gofmt check
make build       # binaries into ./bin
```

### Testing approach

80 test functions, including:

- **Config validation** — table-driven, one specific error per malformed input
- **Weighted selection** — statistical, 100k draws within tolerance of declared weights
- **Latency sampling** — asserts p50 behaves as a median and outliers exceed p99
- **Traversal** — in-memory exporter asserts span counts, parent/child IDs, span kinds, error status
- **Export** — an in-process OTLP gRPC server asserts delta temporality and exponential histograms *on the wire*
- **End-to-end** — compiles the binary, runs it against a live OTLP server, and asserts traces arrive connected

The e2e suite is the meaningful one: it proved a real bug during development.
Sharing one exporter across per-service providers meant the first provider's
shutdown closed the shared transport and every other provider failed to
flush. Exporters are now per-service over one shared gRPC connection.

## Project layout

```
cmd/otel-ecom-load-genrator/   CLI, wiring, signal handling, shutdown
internal/config/               Schema, loading, validation
internal/chaos/                Weighted selection, latency sampling
internal/telemetry/            Providers, Emitter, attribute conversion
internal/engine/               Traversal, rate scheduling, surges
topologies/                    Shipped topology files
test/e2e/                      Binary-level tests against a live OTLP server
docs/superpowers/specs/        Design document
```

## Design notes

Two JSON decoding facts shape the attribute handling, and both are easy to get
wrong:

- `encoding/json` decodes every number to `float64`, so `"status_code": 503`
  becomes a float unless integral values are detected and converted.
- `encoding/json` decodes every array to `[]interface{}`, so a `case []int`
  branch never matches and array attributes are silently dropped.

The spec's `Correctness requirements` section (C1–C12) lists these and ten
other guarantees with the failure mode each prevents. See
[the design doc](docs/superpowers/specs/2026-08-25-otel-ecom-load-genrator-design.md).
