package nftables

import (
	"fmt"
	"iter"
)

type Interval struct {
	Start int
	End   int
}

func (i Interval) String() string {
	return fmt.Sprintf("%d-%d", i.Start, i.End)
}

// EqualWeightIntervals partitions the range [0, length) into len(values) equal intervals,
// assigning successive intervals to each of the values.
//
// There are only length intervals to hand out, so if len(values) > length the
// surplus values are skipped rather than yielded an empty (start > end) interval,
// which is not a valid nftables interval.
func EqualWeightIntervals[Slice ~[]E, E any](values Slice, length int) iter.Seq2[Interval, E] {
	return func(yield func(Interval, E) bool) {
		n := len(values)
		if length <= 0 || n == 0 {
			return
		}
		per, rem := length/n, length%n
		start := 0
		for i, value := range values {
			size := per
			if i < rem {
				size++
			}
			if size == 0 {
				return
			}
			end := start + size - 1
			if !yield(Interval{start, end}, value) {
				return
			}
			start = end + 1
		}
	}
}
