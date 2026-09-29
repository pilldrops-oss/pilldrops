package main

import (
	"math"
	"slices"
)

// percentile returns the p-th percentile (0-100) of xs by nearest-rank,
// index = ceil(p/100 * n) - 1. The convention is recorded in every result
// document as percentileMethod, so changing it changes the schema.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(xs))
	rank := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[min(max(rank, 0), len(s)-1)]
}

// tally counts request outcomes by HTTP status.
type tally struct {
	ok       int
	byStatus map[int]int
}

func (t *tally) add(status int) {
	if t.byStatus == nil {
		t.byStatus = map[int]int{}
	}
	t.byStatus[status]++
	if status >= 200 && status < 300 {
		t.ok++
	}
}
