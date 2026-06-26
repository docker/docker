package nftables

import (
	"maps"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"pgregory.net/rapid"
)

func TestEqualWeightIntervals(t *testing.T) {
	// Zero values do something defensible
	m := maps.Collect(EqualWeightIntervals([]int(nil), 10))
	assert.Check(t, is.Len(m, 0))
	m = maps.Collect(EqualWeightIntervals([]int{1, 2, 3}, 0))
	assert.Check(t, is.Len(m, 0))

	// Iterator is reusable
	it := EqualWeightIntervals([]int{1, 2, 3}, 10)
	m, m2 := maps.Collect(it), maps.Collect(it)
	assert.Check(t, is.DeepEqual(m, m2))

	// More values than intervals: the surplus values are skipped rather than
	// yielded an empty (start > end) interval, which nftables would reject.
	m = maps.Collect(EqualWeightIntervals([]int{1, 2, 3}, 2))
	assert.Check(t, is.Len(m, 2))
	for iv := range m {
		assert.Check(t, iv.Start <= iv.End, "empty interval %s", iv)
	}

	rapid.Check(t, testEqualWeightIntervals)
}

func testEqualWeightIntervals(t *rapid.T) {
	length := rapid.IntRange(1, 65536).Draw(t, "length")
	nvalues := rapid.IntRange(1, length).Draw(t, "nvalues")
	values := make([]int, nvalues)
	for i := range values {
		values[i] = i
	}

	var prev Interval
	for i, v := range EqualWeightIntervals(values, length) {
		if v == 0 {
			assert.Check(t, i.Start == 0)
		} else {
			assert.Check(t, i.Start == prev.End+1)
		}
		prev = i
	}
	assert.Check(t, prev.End == length-1)
}
