package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
)

func TestIntervalFor(t *testing.T) {
	tests := []struct {
		tracesPerHour int
		want          time.Duration
	}{
		{3600, time.Second},
		{7200, 500 * time.Millisecond},
		{60, time.Minute},
		{1, time.Hour},
	}
	for _, tc := range tests {
		if got := intervalFor(tc.tracesPerHour); got != tc.want {
			t.Errorf("intervalFor(%d) = %v, want %v", tc.tracesPerHour, got, tc.want)
		}
	}
}

// A non-positive rate must not produce a zero or negative interval, which
// would make a ticker panic.
func TestIntervalFor_GuardsNonPositive(t *testing.T) {
	for _, n := range []int{0, -1, -3600} {
		if got := intervalFor(n); got <= 0 {
			t.Errorf("intervalFor(%d) = %v, want positive", n, got)
		}
	}
}

// An extreme rate must stay above the floor rather than becoming zero.
func TestIntervalFor_ClampsToFloor(t *testing.T) {
	got := intervalFor(1_000_000_000)
	if got < minInterval {
		t.Errorf("intervalFor(1e9) = %v, want at least %v", got, minInterval)
	}
}

// C8: cancelling the context must stop every generator goroutine, not just one.
func TestRun_CancellationStopsAllGenerators(t *testing.T) {
	roots := []config.RootRoute{
		{Service: "web", Route: "GET /cart", TracesPerHour: 3_600_000},
		{Service: "cart", Route: "GET /items", TracesPerHour: 3_600_000},
		{Service: "redis", Route: "GET", TracesPerHour: 3_600_000},
	}

	var emitted atomic.Int64
	sched := newSchedulerWithEmit(roots, nil, func(ctx context.Context, svc, route string) {
		emitted.Add(1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	// Let it emit, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancellation")
	}

	if emitted.Load() == 0 {
		t.Error("no traces emitted before cancellation")
	}

	// No further emissions after Run returns.
	settled := emitted.Load()
	time.Sleep(50 * time.Millisecond)
	if got := emitted.Load(); got != settled {
		t.Errorf("emissions continued after Run returned: %d -> %d", settled, got)
	}
}

func TestRun_EmitsForEveryRootRoute(t *testing.T) {
	roots := []config.RootRoute{
		{Service: "web", Route: "GET /cart", TracesPerHour: 3_600_000},
		{Service: "cart", Route: "GET /items", TracesPerHour: 3_600_000},
	}

	var mu sync.Mutex
	seen := map[string]int{}
	sched := newSchedulerWithEmit(roots, nil, func(ctx context.Context, svc, route string) {
		mu.Lock()
		seen[svc+" "+route]++
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	sched.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if seen["web GET /cart"] == 0 {
		t.Error("no emissions for the web root route")
	}
	if seen["cart GET /items"] == 0 {
		t.Error("no emissions for the cart root route")
	}
}

// Surge windows must raise the rate for the routes they name.
func TestSurgeMultiplier(t *testing.T) {
	surges := []config.Surge{{
		EveryMinutes:    60,
		DurationSeconds: 30,
		Multiplier:      5,
		Routes:          []string{"GET /cart"},
	}}
	s := newScheduler(nil, surges, nil)

	// At second 0 the window is open.
	if got := s.multiplierAt("GET /cart", 0); got != 5 {
		t.Errorf("multiplier at t=0 = %v, want 5", got)
	}
	// Still inside the 30s window.
	if got := s.multiplierAt("GET /cart", 29*time.Second); got != 5 {
		t.Errorf("multiplier at t=29s = %v, want 5", got)
	}
	// Past the window.
	if got := s.multiplierAt("GET /cart", 31*time.Second); got != 1 {
		t.Errorf("multiplier at t=31s = %v, want 1", got)
	}
	// Next period reopens it.
	if got := s.multiplierAt("GET /cart", 60*time.Minute+time.Second); got != 5 {
		t.Errorf("multiplier at the next period = %v, want 5", got)
	}
	// An unlisted route is never surged.
	if got := s.multiplierAt("GET /other", 0); got != 1 {
		t.Errorf("unlisted route multiplier = %v, want 1", got)
	}
}

// An empty Routes list means the surge applies to every root route.
func TestSurgeMultiplier_EmptyRoutesMeansAll(t *testing.T) {
	surges := []config.Surge{{
		EveryMinutes:    10,
		DurationSeconds: 60,
		Multiplier:      3,
	}}
	s := newScheduler(nil, surges, nil)

	for _, route := range []string{"GET /a", "POST /b"} {
		if got := s.multiplierAt(route, 0); got != 3 {
			t.Errorf("route %q multiplier = %v, want 3", route, got)
		}
	}
}

func TestSurgeMultiplier_NoSurgesIsAlwaysOne(t *testing.T) {
	s := newScheduler(nil, nil, nil)
	if got := s.multiplierAt("anything", 12*time.Second); got != 1 {
		t.Errorf("multiplier = %v, want 1", got)
	}
}

// Overlapping surges compound rather than one silently winning.
func TestSurgeMultiplier_OverlappingSurgesCompound(t *testing.T) {
	surges := []config.Surge{
		{EveryMinutes: 60, DurationSeconds: 60, Multiplier: 2, Routes: []string{"r"}},
		{EveryMinutes: 60, DurationSeconds: 60, Multiplier: 3, Routes: []string{"r"}},
	}
	s := newScheduler(nil, surges, nil)
	if got := s.multiplierAt("r", 0); got != 6 {
		t.Errorf("compounded multiplier = %v, want 6", got)
	}
}
