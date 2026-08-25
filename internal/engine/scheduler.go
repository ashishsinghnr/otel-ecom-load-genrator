package engine

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
)

// minInterval floors the tick interval so an extreme tracesPerHour cannot
// produce a zero-duration ticker, which panics.
const minInterval = 100 * time.Microsecond

// defaultInterval is used when a rate is non-positive. Validation rejects
// those, so this only guards direct callers.
const defaultInterval = time.Hour

// emitFunc is the emission callback, injected so the scheduler can be tested
// without providers.
type emitFunc func(ctx context.Context, service, route string)

// scheduler drives one goroutine per root route at that route's rate,
// applying any active surge multiplier.
type scheduler struct {
	roots  []config.RootRoute
	surges []config.Surge
	emit   emitFunc

	// started is the reference point for surge windows.
	started time.Time
}

func newScheduler(roots []config.RootRoute, surges []config.Surge, emit emitFunc) *scheduler {
	return &scheduler{roots: roots, surges: surges, emit: emit, started: time.Now()}
}

// newSchedulerWithEmit is the constructor used by tests.
func newSchedulerWithEmit(roots []config.RootRoute, surges []config.Surge, emit emitFunc) *scheduler {
	return newScheduler(roots, surges, emit)
}

// intervalFor converts a per-hour rate into a tick interval.
func intervalFor(tracesPerHour int) time.Duration {
	if tracesPerHour <= 0 {
		return defaultInterval
	}
	d := time.Hour / time.Duration(tracesPerHour)
	if d < minInterval {
		return minInterval
	}
	return d
}

// multiplierAt returns the combined surge multiplier for a route at the given
// offset from start. Overlapping surges compound.
func (s *scheduler) multiplierAt(route string, since time.Duration) float64 {
	mult := 1.0
	for _, surge := range s.surges {
		if !surgeApplies(surge, route) {
			continue
		}
		period := time.Duration(surge.EveryMinutes) * time.Minute
		if period <= 0 {
			continue
		}
		// Open at the start of each period, for DurationSeconds.
		if since%period < time.Duration(surge.DurationSeconds)*time.Second {
			mult *= surge.Multiplier
		}
	}
	return mult
}

// surgeApplies reports whether a surge covers a route. An empty Routes list
// means every root route.
func surgeApplies(surge config.Surge, route string) bool {
	if len(surge.Routes) == 0 {
		return true
	}
	for _, r := range surge.Routes {
		if r == route {
			return true
		}
	}
	return false
}

// Run drives generation until ctx is cancelled, then waits for the
// per-route goroutines to stop.
//
// Cancellation propagates through the context to every goroutine, so all of
// them stop rather than only the one that happens to receive a signal
// (spec C8).
func (s *scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, root := range s.roots {
		wg.Add(1)
		go func(rr config.RootRoute) {
			defer wg.Done()
			s.runRoute(ctx, rr)
		}(root)
	}
	wg.Wait()
}

// runRoute ticks one root route, re-evaluating the surge multiplier each tick
// so a window can open and close without restarting the goroutine.
func (s *scheduler) runRoute(ctx context.Context, root config.RootRoute) {
	base := intervalFor(root.TracesPerHour)
	current := s.effectiveInterval(base, root.Route)

	ticker := time.NewTicker(current)
	defer ticker.Stop()

	// Emissions run in their own goroutines so a slow trace cannot delay the
	// next tick; this WaitGroup keeps them from outliving Run.
	var inflight sync.WaitGroup
	defer inflight.Wait()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			inflight.Add(1)
			go func() {
				defer inflight.Done()
				s.emit(ctx, root.Service, root.Route)
			}()

			// Adjust the ticker when a surge window opens or closes.
			if next := s.effectiveInterval(base, root.Route); next != current {
				current = next
				ticker.Reset(current)
			}
		}
	}
}

// effectiveInterval divides the base interval by the active multiplier.
func (s *scheduler) effectiveInterval(base time.Duration, route string) time.Duration {
	mult := s.multiplierAt(route, time.Since(s.started))
	if mult <= 1 {
		return base
	}
	d := time.Duration(float64(base) / mult)
	if d < minInterval {
		return minInterval
	}
	return d
}

// Run starts generation for every root route and blocks until ctx is
// cancelled. It is the Engine's entry point for continuous load.
//
// When reportEvery is positive, a progress line is logged at that interval so
// a long run is observable without waiting for the backend to show data.
func (e *Engine) Run(ctx context.Context, roots []config.RootRoute, surges []config.Surge, reportEvery time.Duration) {
	if reportEvery > 0 {
		stop := e.startReporter(ctx, reportEvery)
		defer stop()
	}

	sched := newScheduler(roots, surges, e.EmitTrace)
	sched.Run(ctx)
}

// startReporter logs cumulative and per-interval counts until ctx is done.
// The returned function waits for the reporter to finish.
func (e *Engine) startReporter(ctx context.Context, every time.Duration) func() {
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(every)
		defer ticker.Stop()

		started := time.Now()
		var last Stats

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				now := e.Stats()
				elapsed := time.Since(started).Round(time.Second)

				// Per-interval deltas make a stalled run obvious; totals alone
				// keep rising and hide it.
				e.log.Info("progress",
					"elapsed", elapsed.String(),
					"traces", now.Traces,
					"spans", now.Spans,
					"logs", now.Logs,
					"errorSpans", now.ErrorSpans,
					"tracesPerInterval", now.Traces-last.Traces,
					"spansPerInterval", now.Spans-last.Spans,
					"spansPerSec", perSecond(now.Spans-last.Spans, every),
				)
				last = now
			}
		}
	}()

	return func() { <-done }
}

// perSecond converts a per-interval count into a rounded rate.
func perSecond(delta int64, interval time.Duration) float64 {
	if interval <= 0 {
		return 0
	}
	rate := float64(delta) / interval.Seconds()
	return math.Round(rate*10) / 10
}
