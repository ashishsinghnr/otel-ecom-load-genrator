package chaos

import (
	"math"
	"testing"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
)

// C1: bounds helpers must never panic, whatever the inputs.
func TestIntn_BoundaryValues(t *testing.T) {
	cases := []int{0, 1, 2, 5, 100}
	for _, n := range cases {
		got := Intn(n)
		if n <= 1 {
			if got != 0 {
				t.Fatalf("Intn(%d) = %d, want 0", n, got)
			}
			continue
		}
		if got < 0 || got >= n {
			t.Fatalf("Intn(%d) = %d, out of range [0,%d)", n, got, n)
		}
	}
}

// The reference implementation this design learns from panics here.
func TestIntn_NegativeAndZeroDoNotPanic(t *testing.T) {
	for _, n := range []int{-100, -1, 0} {
		if got := Intn(n); got != 0 {
			t.Fatalf("Intn(%d) = %d, want 0", n, got)
		}
	}
}

func TestIntRange_EqualAndInvertedBounds(t *testing.T) {
	if got := IntRange(5, 5); got != 5 {
		t.Fatalf("IntRange(5,5) = %d, want 5", got)
	}
	// Inverted bounds must be handled, not panic.
	if got := IntRange(10, 2); got < 2 || got > 10 {
		t.Fatalf("IntRange(10,2) = %d, want within [2,10]", got)
	}
	for i := 0; i < 1000; i++ {
		if got := IntRange(3, 7); got < 3 || got > 7 {
			t.Fatalf("IntRange(3,7) = %d, out of range", got)
		}
	}
}

// C7/C11: weighted selection must respect declared weights.
func TestPickWeighted_RespectsDistribution(t *testing.T) {
	weights := []int{10, 30, 60}
	const draws = 100_000

	counts := make([]int, len(weights))
	for i := 0; i < draws; i++ {
		idx := PickWeighted(weights)
		if idx < 0 || idx >= len(weights) {
			t.Fatalf("PickWeighted returned out-of-range index %d", idx)
		}
		counts[idx]++
	}

	total := 0
	for _, w := range weights {
		total += w
	}
	for i, w := range weights {
		want := float64(w) / float64(total)
		got := float64(counts[i]) / float64(draws)
		if math.Abs(got-want) > 0.01 {
			t.Errorf("index %d: share %.4f, want %.4f (tolerance 0.01)", i, got, want)
		}
	}
}

// Weights summing to something other than 100 are normalized, not clamped.
func TestPickWeighted_NormalizesNonHundredSums(t *testing.T) {
	weights := []int{25, 85} // sums to 110
	const draws = 100_000

	counts := make([]int, 2)
	for i := 0; i < draws; i++ {
		counts[PickWeighted(weights)]++
	}

	want := 25.0 / 110.0
	got := float64(counts[0]) / float64(draws)
	if math.Abs(got-want) > 0.01 {
		t.Errorf("share %.4f, want %.4f", got, want)
	}
}

func TestPickWeighted_EdgeCases(t *testing.T) {
	if got := PickWeighted(nil); got != -1 {
		t.Errorf("PickWeighted(nil) = %d, want -1", got)
	}
	if got := PickWeighted([]int{}); got != -1 {
		t.Errorf("PickWeighted(empty) = %d, want -1", got)
	}
	if got := PickWeighted([]int{0, 0}); got != -1 {
		t.Errorf("PickWeighted(all zero) = %d, want -1", got)
	}
	if got := PickWeighted([]int{7}); got != 0 {
		t.Errorf("PickWeighted(single) = %d, want 0", got)
	}
	// A zero-weight entry must never be selected.
	for i := 0; i < 5000; i++ {
		if PickWeighted([]int{0, 5}) != 1 {
			t.Fatal("selected a zero-weight entry")
		}
	}
}

func TestPickAttributeSet(t *testing.T) {
	sets := []config.AttributeSet{
		{Weight: 0, Attributes: map[string]interface{}{"never": true}},
		{Weight: 10, Attributes: map[string]interface{}{"always": true}},
	}
	for i := 0; i < 2000; i++ {
		got := PickAttributeSet(sets)
		if got == nil {
			t.Fatal("expected a set")
		}
		if _, ok := got.Attributes["always"]; !ok {
			t.Fatal("picked the zero-weight set")
		}
	}
	if PickAttributeSet(nil) != nil {
		t.Error("expected nil for empty input")
	}
}

func TestPickEventSet(t *testing.T) {
	sets := []config.EventSet{
		{Weight: 5, Events: []config.Event{{Name: "a"}}},
	}
	got := PickEventSet(sets)
	if got == nil || len(got.Events) != 1 || got.Events[0].Name != "a" {
		t.Fatalf("unexpected event set: %+v", got)
	}
	if PickEventSet(nil) != nil {
		t.Error("expected nil for empty input")
	}
}

// Latency sampling must land in the declared distribution and honor outliers.
func TestSampleLatency_Distribution(t *testing.T) {
	l := config.Latency{P50: 100, P99: 500, OutlierRate: 0, OutlierMultiplier: 1}
	const draws = 20_000

	var below, above int
	min, max := math.MaxInt, 0
	for i := 0; i < draws; i++ {
		d := SampleLatencyMillis(l)
		if d < min {
			min = d
		}
		if d > max {
			max = d
		}
		if d <= l.P50 {
			below++
		}
		if d > l.P99 {
			above++
		}
	}

	if min < 1 {
		t.Errorf("sampled a duration below 1ms: %d", min)
	}
	if above > 0 {
		t.Errorf("%d samples exceeded p99 with outliers disabled", above)
	}
	// Roughly half should fall at or below p50.
	share := float64(below) / float64(draws)
	if share < 0.35 || share > 0.65 {
		t.Errorf("share at/below p50 = %.3f, want roughly 0.5", share)
	}
}

func TestSampleLatency_FixedWhenPercentilesEqual(t *testing.T) {
	l := config.Latency{P50: 42, P99: 42, OutlierMultiplier: 1}
	for i := 0; i < 500; i++ {
		if got := SampleLatencyMillis(l); got != 42 {
			t.Fatalf("SampleLatencyMillis = %d, want exactly 42", got)
		}
	}
}

func TestSampleLatency_OutliersExceedP99(t *testing.T) {
	l := config.Latency{P50: 10, P99: 20, OutlierRate: 0.5, OutlierMultiplier: 10}
	const draws = 5000

	outliers := 0
	for i := 0; i < draws; i++ {
		if SampleLatencyMillis(l) > l.P99 {
			outliers++
		}
	}
	share := float64(outliers) / float64(draws)
	if share < 0.35 || share > 0.65 {
		t.Errorf("outlier share = %.3f, want roughly 0.5", share)
	}
}

// A zero-value Latency must still produce a usable duration rather than
// panicking or returning zero.
func TestSampleLatency_ZeroValueIsSafe(t *testing.T) {
	if got := SampleLatencyMillis(config.Latency{}); got < 1 {
		t.Fatalf("SampleLatencyMillis(zero) = %d, want at least 1", got)
	}
}

func TestPickInstance(t *testing.T) {
	instances := []string{"a", "b", "c"}
	seen := map[string]bool{}
	for i := 0; i < 3000; i++ {
		got := PickInstance(instances)
		if got == "" {
			t.Fatal("empty instance")
		}
		seen[got] = true
	}
	if len(seen) != 3 {
		t.Errorf("saw %d distinct instances, want 3", len(seen))
	}
	if got := PickInstance(nil); got != "" {
		t.Errorf("PickInstance(nil) = %q, want empty", got)
	}
	if got := PickInstance([]string{"only"}); got != "only" {
		t.Errorf("PickInstance(single) = %q", got)
	}
}

func TestFloat64_InUnitInterval(t *testing.T) {
	for i := 0; i < 10_000; i++ {
		f := Float64()
		if f < 0 || f >= 1 {
			t.Fatalf("Float64() = %v, out of [0,1)", f)
		}
	}
}
