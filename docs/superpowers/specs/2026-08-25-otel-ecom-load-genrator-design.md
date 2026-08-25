# otel-ecom-load-genrator — Design

**Date:** 2026-08-25
**Status:** Approved for planning
**Module:** `github.com/ashishsinghnr/otel-ecom-load-genrator`

## Purpose

A topology-driven synthetic OpenTelemetry load generator for an e-commerce shopping
domain. It reads a JSON file describing simulated services, their routes, and the calls
between them, then continuously emits traces, span events, metrics, and logs over OTLP.

The primary target is a real backend — New Relic — so the generator produces data
volume and shape suitable for building dashboards, testing alerts, and demos without
deploying an actual application.

The declarative-topology approach follows a pattern common to synthetic telemetry
generators: describe the system's shape in configuration, then let the generator walk it.
The [Correctness requirements](#correctness-requirements) section lists the specific
behaviors this implementation guarantees, several of which are places where naive
implementations of this pattern tend to go wrong.

## Goals

- Emit all four signal flavors — traces, span events, metrics, logs — from one traversal,
  so the signals agree with each other.
- Export correctly to New Relic on first run, including the settings that are easy to get
  silently wrong (delta temporality, `api-key` header).
- Make an invalid topology a startup error, never silently-missing telemetry.
- Produce non-flat dashboards: weighted error injection, latency outliers, traffic surges.
- Stay comprehensible: small packages with single responsibilities, each testable alone.

## Non-Goals

- Not a real application. No business logic is executed; all telemetry is synthetic.
- Not a collector component. It is a standalone binary that speaks OTLP.
- Not a benchmark harness. It generates load; it does not measure the backend.
- Not wall-clock accurate. Spans do not sleep for their reported duration (see [Timing](#timing)).

## Architecture

```
cmd/otel-ecom-load-genrator/main.go   CLI parsing, wiring, signal handling, shutdown
internal/config/                       JSON schema structs; load + validate
internal/telemetry/                    Emitter: per-service tracer + meter + logger
internal/engine/                       traversal, rate scheduling, timing
internal/chaos/                        weighted selection, latency model, surge scheduler
topologies/shop-full.json              ~16-service e-commerce topology
topologies/shop-smoke.json             3-service topology for pipeline verification
docker-compose.yml                     collector + Jaeger, for local runs without New Relic
```

### Data flow

1. `main` parses flags, loads the topology JSON, and runs validation. Validation failure
   exits non-zero before any exporter is constructed.
2. For each service in the topology, construct one `Emitter` holding a tracer, meter, and
   logger that share a single `resource`.
3. For each entry in `rootRoutes`, start one goroutine driven by a rate scheduler.
4. On each tick, traverse the topology graph from that root route, emitting a span per
   visited route and recursing into `downstreamCalls`.
5. Spans, metrics, and logs batch to OTLP exporters.
6. On SIGINT/SIGTERM, stop schedulers, then flush and shut down every provider with a
   bounded timeout.

### Package responsibilities

| Package | Does | Depends on |
|---|---|---|
| `config` | Defines the JSON schema; loads and validates it | nothing (pure) |
| `chaos` | Weighted selection, latency sampling, surge state | `config` |
| `telemetry` | Builds providers/exporters; exposes `Emitter` | `config` |
| `engine` | Walks the topology, drives rates, emits via `Emitter` | `config`, `chaos`, `telemetry` |
| `cmd` | Flags, wiring, lifecycle | all of the above |

`config` and `chaos` are pure and testable with no exporter. `engine` is testable against
the SDK's in-memory exporter.

## Signals

Every `Emitter` shares one `resource` carrying `service.name`, `service.namespace`,
`service.instance.id`, and `deployment.environment`.

### Traces

- One span per visited route, named by the route.
- `SpanKind` from config (`server`, `client`, `producer`, `consumer`, `internal`).
- Parent/child linkage via Go `context` propagation.
- Weighted attribute sets applied per span.
- Errors set `span.SetStatus(codes.Error, reason)` **and** `error.type` **and**
  `http.response.status_code` — so backends classify them as errors.

### Span events

In-span events with attributes and timestamps, e.g. `payment.authorized`,
`inventory.reserved`, `cart.item_added`, `fraud.check.completed`. Selected by weighted
event sets, same mechanism as attributes.

### Metrics

Emitted from the same traversal as the spans, so metric and trace values agree.

**RED (technical):**
- `http.server.request.duration` — exponential histogram, seconds
- `http.server.request.count` — counter
- `http.server.error.count` — counter

**Business:**
- `orders.placed` — counter
- `cart.value` — histogram, USD
- `payment.failures` — counter
- `checkout.conversion` — counter pair (started/completed)

### Logs

Log records are emitted inside span context, so `trace_id` and `span_id` attach
automatically and the backend supports trace↔log pivoting. Failed spans additionally emit
an ERROR-severity record naming the failure reason.

**Version note:** the OTel Go logs SDK is `go.opentelemetry.io/otel/sdk/log v0.22.0` —
still beta, while traces and metrics are stable at `v1.46.0`. The logs pipeline is isolated
behind a small internal interface so future breaking changes touch one file.

## Exporters and backend configuration

A `--backend` flag selects defaults:

| `--backend` | Endpoint | Notes |
|---|---|---|
| `newrelic` (default) | `https://staging-otlp.nr-data.net:4317` | US region; `api-key` header; delta temporality; exponential histograms; gzip |
| `otlp` | from `OTEL_EXPORTER_OTLP_*` | No opinion; standard env vars only |
| `local` | `http://localhost:4317` | Insecure, for the bundled docker-compose |

New Relic requirements, confirmed against New Relic documentation:

- **`api-key` header** — read from the `NEW_RELIC_LICENSE_KEY` environment variable.
  Never written to any file, never logged, never a CLI flag (flags leak into shell history
  and process listings).
- **Delta temporality** — required. The SDK default is cumulative, which silently produces
  wrong values in New Relic. Set explicitly on the metric reader.
- **Exponential histograms** — preferred by New Relic for long-tailed distributions such
  as request duration.
- **US region** is the default. `--nr-region eu` switches to `otlp.eu01.nr-data.net`.
  Sending to the wrong region fails authentication even with a valid key.

Standard `OTEL_EXPORTER_OTLP_*` environment variables are still honored and take
precedence over `--backend` defaults, so the tool stays usable with any OTLP consumer.

## Topology schema

```json
{
  "topology": {
    "services": [
      {
        "serviceName": "checkout",
        "instances": ["checkout-7d9f-abc", "checkout-7d9f-def"],
        "tier": "application",
        "attributeSets": [
          { "weight": 85, "attributes": { "version": "v2.1", "region": "us-east-1" } },
          { "weight": 15, "attributes": { "version": "v2.0", "region": "us-east-1" } }
        ],
        "eventSets": [
          { "weight": 1, "events": [{ "name": "cart.validated", "attributes": { "items": 3 } }] }
        ],
        "routes": [
          {
            "route": "POST /checkout",
            "spanKind": "server",
            "downstreamCalls": {
              "payment": "POST /authorize",
              "inventory": "POST /reserve"
            },
            "latency": {
              "p50": 120,
              "p99": 900,
              "outlierRate": 0.01,
              "outlierMultiplier": 8
            },
            "attributeSets": [
              { "weight": 3,  "attributes": { "error": true, "http.response.status_code": 503 } },
              { "weight": 97, "attributes": { "http.response.status_code": 200 } }
            ],
            "metrics": {
              "business": [{ "name": "orders.placed", "kind": "counter" }]
            }
          }
        ]
      }
    ]
  },
  "rootRoutes": [
    { "service": "web-bff", "route": "POST /checkout", "tracesPerHour": 3000 }
  ],
  "surges": [
    {
      "everyMinutes": 15,
      "durationSeconds": 120,
      "multiplier": 6,
      "routes": ["POST /checkout"]
    }
  ]
}
```

### Schema notes

- **`latency`** is a distribution, not a single ceiling value. Sampling between `p50` and
  `p99` with an outlier tail produces a realistic histogram instead of a uniform one.
- **`weight`** must be a positive integer. Weights are normalized against their actual sum,
  so a set declared `25` and `85` behaves as 22.7% / 77.3% — and validation warns that the
  weights do not sum to 100 in case that was unintended.
- **`instances`** are sampled per span to populate `service.instance.id`.
- **`surges`** implement flash-sale bursts: for `durationSeconds` every `everyMinutes`, the
  listed routes' rates multiply by `multiplier`.
- **`error: true`** inside an attribute set is a *directive*, not a literal attribute. When a
  set carrying it is selected, the implementation sets span status to `Error`, sets
  `error.type`, and emits an ERROR log record. The key itself is not exported as a boolean
  attribute. This is what makes the backend classify the span as failed.

### Validation rules

Validation runs before any exporter is constructed. Each failure names the offending path.

1. Every `downstreamCalls` key resolves to a declared `serviceName`.
2. Every `downstreamCalls` value resolves to a `route` on that service.
3. Every `rootRoutes` entry resolves to a real service + route.
4. The call graph is acyclic. A self-referencing service is allowed only when it targets a
   different route and the overall graph remains acyclic.
5. `tracesPerHour` > 0.
6. Each `attributeSets` / `eventSets` list either is empty or contains at least one entry
   with `weight` > 0.
7. `latency.p50` >= 1, `latency.p99` >= `latency.p50`, `outlierRate` in [0, 1),
   `outlierMultiplier` >= 1.
8. `serviceName` values are unique; `route` values are unique within a service.
9. Warning (not fatal): a declared service unreachable from any root route.

## Chaos behaviors

| Behavior | Mechanism | Config |
|---|---|---|
| Weighted error injection | Attribute set carrying `error: true` selected by weight; sets span status, `error.type`, status code, and emits an ERROR log | `attributeSets` weights |
| Latency outliers | With probability `outlierRate`, multiply sampled duration by `outlierMultiplier` | `latency.outlierRate` |
| Traffic surges | Rate scheduler multiplies tick frequency for listed routes during a surge window | `surges` |

## Timing

**Decision: synthetic end times.** Spans do not sleep for their reported duration; an end
timestamp is computed and applied. This keeps throughput high and CPU cost low.

**Accepted consequence:** because children are emitted concurrently with independently
computed end times, a parent span can end before its children. Waterfall views in New Relic
will sometimes show children extending past their parent, and critical-path analysis is not
trustworthy on this data.

**Mitigation:** all end-time computation lives in one function in `engine`, selected by a
`--timing synthetic|nested` flag defaulting to `synthetic`. Implementing correctly-nested
durations later is a change to one function, not a rewrite. `nested` is out of scope for the
first implementation.

## Shipped topologies

**`shop-full.json`** — ~16 services across tiers:

- *Edge:* `web-bff`, `auth`
- *Discovery:* `search`, `catalog`, `pricing`, `promo`, `reviews`
- *Purchase:* `cart`, `checkout`, `payment`, `fraud`
- *Fulfillment:* `inventory`, `order-mgmt`, `shipping`
- *Async:* `order-events` (producer/consumer span kinds), `notification`
- *Data tier:* `postgres`, `redis` as client-kind leaf calls

Root routes cover browse, search, add-to-cart, checkout, and order-status flows at
differing rates.

**`shop-smoke.json`** — 3 services, one root route, high rate. Verifies a pipeline in
seconds.

## CLI

```
otel-ecom-load-genrator [options]

  --topology string     Path to topology JSON (required)
  --backend string      newrelic | otlp | local (default "newrelic")
  --nr-region string    us | eu (default "us")
  --namespace string    service.namespace resource attribute (default "ecom")
  --environment string  deployment.environment (default "synthetic")
  --timing string       synthetic | nested (default "synthetic")
  --duration duration   Stop after this long (default 0 = run until interrupted)
  --validate-only       Validate the topology and exit
  --pprof string        pprof listen address (default "" = disabled)
  --log-level string    debug | info | warn | error (default "info")
```

Environment: `NEW_RELIC_LICENSE_KEY` (required when `--backend newrelic`), plus standard
`OTEL_EXPORTER_OTLP_*`.

## Error handling

- **Startup errors** (unreadable topology, validation failure, missing license key) print a
  specific message and exit non-zero. No partial startup.
- **Export errors** are logged at WARN and retried by the SDK's built-in retry. They never
  terminate the process — a backend blip should not kill a long-running generator.
- **Per-emission panics** are recovered per goroutine, logged with the service and route,
  and counted in an internal `generator.panics` counter, so one bad route cannot take the
  process down.
- **Shutdown** stops schedulers, then calls `ForceFlush` and `Shutdown` on every provider
  with a bounded context (default 10s) so batched telemetry is not lost. Skipping the flush
  discards whatever the batch processors are still holding.

## Testing strategy

Development follows TDD. Tests are the deliverable, not an afterthought.

| Area | Approach |
|---|---|
| `config` validation | Table-driven: dangling refs, cycles, zero weights, bad latency, duplicate names — each asserts a specific error |
| Weighted selection | Statistical: 100k draws land within tolerance of declared weights; explicitly covers single-element, empty, and boundary cases |
| Latency sampling | Distribution assertions: p50/p99 land near declared values; outlier rate within tolerance |
| Traversal | SDK `tracetest.InMemoryExporter`: assert span count, parent/child ID linkage, span kinds, and that error attribute sets set span status |
| Metrics | SDK in-memory metric reader: assert instruments recorded with expected values and delta temporality |
| Surge scheduler | Fake clock: assert rate multiplies during the window and restores after |
| Shipped topologies | Both JSON files load and pass validation |
| End-to-end | `docker-compose up` (collector + Jaeger), run `shop-smoke.json`, assert spans arrive |

## Correctness requirements

These are explicit guarantees the implementation must meet. Each names a failure mode that
topology-walking generators are prone to, and the required behavior. They exist as a
checklist for implementation and review.

| # | Requirement | Failure mode it prevents |
|---|---|---|
| C1 | Random-range helpers must handle equal and inverted bounds without panicking, and must never discard the error from `crypto/rand` | `rand.Int` with a non-positive bound returns `nil`, so a discarded error becomes a nil dereference on the next line — triggered by a single-element list or a latency ceiling of 1 |
| C2 | No sampler is installed by default; any sampling is explicit configuration | A sampler that returns `RecordOnly` marks spans recorded but not exported, silently producing orphaned, incomplete traces |
| C3 | Every `downstreamCalls` service and route reference resolves at validation time, or startup fails | A typo yields no telemetry at all, with no error — the hardest class of bug to notice |
| C4 | The call graph is checked for cycles at validation time | A cyclic topology recurses without bound and spawns goroutines until the process dies |
| C5 | Attribute conversion handles `[]interface{}` and coerces element types | `encoding/json` decodes every array to `[]interface{}`, so typed-slice branches never match and array attributes are silently dropped |
| C6 | Integral `float64` values convert to integer attributes | `encoding/json` decodes all numbers to `float64`, so `http.response.status_code: 503` would export as a float |
| C7 | Each attribute/event set list must contain at least one positive weight | An omitted `weight` defaults to zero, making that set permanently unselectable with no warning |
| C8 | Shutdown cancels all goroutines via context, then flushes every provider under a bounded timeout | Signaling on an unbuffered channel wakes only one receiver; returning from `main` without flushing discards batched telemetry |
| C9 | Both `SIGINT` and `SIGTERM` initiate graceful shutdown | Handling only `SIGINT` means container runtimes and Kubernetes kill the process before it flushes |
| C10 | Failed spans set `span.SetStatus(codes.Error, …)`, `error.type`, and a status-code attribute | An `error: true` attribute alone is not recognized by backends, so error rates read as zero |
| C11 | Weights are normalized against their actual sum, and a sum other than 100 is reported | Weights of 25 and 85 read as "25%" but behave as 22.7%, quietly skewing injected error rates |
| C12 | Every emission goroutine recovers from panics, logs the service and route, and increments a counter | One malformed route otherwise terminates the whole generator |

Dependency baseline: Go 1.24, OTel Go `v1.46.0` (traces, metrics), `sdk/log v0.22.0` (beta).

## Open questions

None blocking. Deferred by decision:

- Correctly-nested span timing (`--timing nested`) — designed for, not implemented initially.
- Metrics-only or logs-only modes — add if the need appears.
- Kubernetes deployment manifests — out of scope; the binary and compose file suffice.
