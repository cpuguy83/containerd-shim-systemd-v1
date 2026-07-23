package main

import (
	"math"
	"sort"
)

// summarize computes latency distribution stats (in ms) from a slice of
// durations expressed as float64 milliseconds. keepRaw controls whether the
// raw samples are retained in the result (useful for regenerating charts).
func summarize(msValues []float64, keepRaw bool) OpStats {
	n := len(msValues)
	if n == 0 {
		return OpStats{}
	}
	sorted := make([]float64, n)
	copy(sorted, msValues)
	sort.Float64s(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}
	mean := sum / float64(n)

	var sq float64
	for _, v := range sorted {
		d := v - mean
		sq += d * d
	}
	stddev := 0.0
	if n > 1 {
		stddev = math.Sqrt(sq / float64(n-1))
	}

	st := OpStats{
		Count:  n,
		Min:    sorted[0],
		P50:    percentile(sorted, 50),
		P90:    percentile(sorted, 90),
		P99:    percentile(sorted, 99),
		Max:    sorted[n-1],
		Mean:   mean,
		Stddev: stddev,
	}
	if keepRaw {
		st.Raw = msValues
	}
	return st
}

// percentile returns the p-th percentile (0-100) of an already-sorted slice
// using linear interpolation between closest ranks.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	rank := (p / 100) * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
