// Package chaos provides the randomness primitives that shape generated
// telemetry: weighted selection, latency sampling, and outlier injection.
//
// Every helper here is total: no input combination panics, and no error from
// the underlying random source is discarded. This is deliberate. Naive
// implementations of these helpers panic on equal or inverted bounds, which
// surfaces as a crash only for certain topologies.
package chaos

import (
	"math"
	"math/rand/v2"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
)

// All helpers use math/rand/v2's top-level functions, which are goroutine-safe
// and auto-seeded. Load generation needs volume, not unpredictability, so a
// PRNG is the right tool here.

// Intn returns a pseudo-random int in [0, n). It returns 0 for n <= 1 rather
// than panicking, which is what makes single-element lists and degenerate
// ranges safe (spec C1).
func Intn(n int) int {
	if n <= 1 {
		return 0
	}
	return rand.IntN(n)
}

// IntRange returns a pseudo-random int in [lo, hi] inclusive. Equal bounds
// return that value; inverted bounds are swapped rather than panicking.
func IntRange(lo, hi int) int {
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo == hi {
		return lo
	}
	return lo + Intn(hi-lo+1)
}

// Float64 returns a pseudo-random float in [0, 1).
func Float64() float64 {
	return rand.Float64()
}

// PickWeighted returns the index selected by cumulative weight, or -1 when
// the input is empty or every weight is zero. Weights are normalized against
// their actual sum, so they need not total 100 (spec C11).
func PickWeighted(weights []int) int {
	total := 0
	for _, w := range weights {
		if w > 0 {
			total += w
		}
	}
	if total == 0 {
		return -1
	}

	r := Intn(total)
	for i, w := range weights {
		if w <= 0 {
			continue
		}
		r -= w
		if r < 0 {
			return i
		}
	}
	// Unreachable: the loop above consumes the whole range.
	return len(weights) - 1
}

// PickAttributeSet returns the weighted-random attribute set, or nil.
func PickAttributeSet(sets []config.AttributeSet) *config.AttributeSet {
	if len(sets) == 0 {
		return nil
	}
	weights := make([]int, len(sets))
	for i, s := range sets {
		weights[i] = s.Weight
	}
	idx := PickWeighted(weights)
	if idx < 0 {
		return nil
	}
	return &sets[idx]
}

// PickEventSet returns the weighted-random event set, or nil.
func PickEventSet(sets []config.EventSet) *config.EventSet {
	if len(sets) == 0 {
		return nil
	}
	weights := make([]int, len(sets))
	for i, s := range sets {
		weights[i] = s.Weight
	}
	idx := PickWeighted(weights)
	if idx < 0 {
		return nil
	}
	return &sets[idx]
}

// PickInstance returns a random instance id, or "" when none are declared.
func PickInstance(instances []string) string {
	if len(instances) == 0 {
		return ""
	}
	return instances[Intn(len(instances))]
}

// SampleLatencyMillis samples a duration in milliseconds from the configured
// distribution.
//
// p50 is treated as a true median: half of all samples fall at or below it.
// The lower half is drawn log-uniformly over [p50/4, p50] and the upper half
// over (p50, p99], which reproduces the right-skewed shape real latency has
// rather than the flat shape a uniform draw over [0, max] gives. With
// probability OutlierRate the result is multiplied by OutlierMultiplier to
// create a tail beyond p99.
//
// The result is always at least 1ms.
func SampleLatencyMillis(l config.Latency) int {
	p50, p99 := l.P50, l.P99
	if p50 < 1 {
		p50 = 1
	}
	if p99 < p50 {
		p99 = p50
	}

	var ms float64
	switch {
	case p99 == p50:
		// Degenerate distribution: a fixed duration.
		ms = float64(p50)
	case Float64() < 0.5:
		// Lower half: log-uniform over [p50/4, p50], so the median lands on p50.
		lo := math.Log(math.Max(1, float64(p50)/4))
		hi := math.Log(float64(p50))
		ms = math.Exp(lo + Float64()*(hi-lo))
	default:
		// Upper half: log-uniform over (p50, p99].
		lo, hi := math.Log(float64(p50)), math.Log(float64(p99))
		ms = math.Exp(lo + Float64()*(hi-lo))
	}

	if l.OutlierRate > 0 && l.OutlierMultiplier > 1 && Float64() < l.OutlierRate {
		ms *= l.OutlierMultiplier
		// Guarantee an outlier is visibly beyond p99 even after rounding.
		if int(ms) <= p99 {
			ms = float64(p99 + 1)
		}
	}

	out := int(math.Round(ms))
	if out < 1 {
		out = 1
	}
	return out
}
