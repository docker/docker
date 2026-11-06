package jobs

import (
	"fmt"
	"math"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestParseCron(t *testing.T) {
	for _, tc := range []struct {
		doc       string
		expr      string
		expectErr string
	}{
		{doc: "every minute", expr: "* * * * *"},
		{doc: "daily at 3am", expr: "0 3 * * *"},
		{doc: "lists ranges steps", expr: "*/15 0-6 1,15 */2 1-5"},
		{doc: "sunday as 7", expr: "0 12 * * 7"},
		{doc: "range with step", expr: "10-50/10 * * * *"},

		{doc: "too few fields", expr: "* * * *", expectErr: "must have 5 fields"},
		{doc: "too many fields", expr: "* * * * * *", expectErr: "must have 5 fields"},
		{doc: "not a number", expr: "a * * * *", expectErr: "not a number"},
		{doc: "minute out of range", expr: "60 * * * *", expectErr: "outside 0-59"},
		{doc: "hour out of range", expr: "* 24 * * *", expectErr: "outside 0-23"},
		{doc: "day-of-month zero", expr: "* * 0 * *", expectErr: "outside 1-31"},
		{doc: "month out of range", expr: "* * * 13 *", expectErr: "outside 1-12"},
		{doc: "weekday out of range", expr: "* * * * 8", expectErr: "outside 0-7"},
		{doc: "reversed range", expr: "30-10 * * * *", expectErr: "range is reversed"},
		{doc: "zero step", expr: "*/0 * * * *", expectErr: "invalid step"},
		{doc: "step on single value", expr: "3/5 * * * *", expectErr: "a step requires * or a range"},
		{doc: "month name", expr: "* * * jan *", expectErr: "not a number"},
		{doc: "weekday name", expr: "* * * * mon", expectErr: "not a number"},
		{doc: "shortcut", expr: "@daily", expectErr: "must have 5 fields"},
		{doc: "double range", expr: "1-2-3 * * * *", expectErr: "not a number"},
		{doc: "empty step", expr: "*/ * * * *", expectErr: "invalid step"},
		{doc: "step without range", expr: "/5 * * * *", expectErr: "a step requires * or a range"},
		{doc: "open-ended range", expr: "5- * * * *", expectErr: "not a number"},
		{doc: "open-started range", expr: "-5 * * * *", expectErr: "not a number"},
		{doc: "empty list entry", expr: "1,,2 * * * *", expectErr: "not a number"},
		{doc: "leading sign", expr: "+5 * * * *", expectErr: "not a number"},
		{doc: "leading sign in step", expr: "*/+5 * * * *", expectErr: "invalid step"},
	} {
		t.Run(tc.doc, func(t *testing.T) {
			_, err := parseCron(tc.expr)
			if tc.expectErr == "" {
				assert.NilError(t, err)
			} else {
				assert.Check(t, is.ErrorContains(err, tc.expectErr))
			}
		})
	}
}

func TestParseCronLargeStep(t *testing.T) {
	t.Parallel()

	schedule, err := parseCron(fmt.Sprintf("0 0 */%d * *", math.MaxInt))
	assert.NilError(t, err)
	assert.Equal(t, schedule.dom, uint64(1<<1))
}

func TestCronNext(t *testing.T) {
	utc := func(y int, mo time.Month, d, h, mi int) time.Time {
		return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
	}
	for _, tc := range []struct {
		doc   string
		expr  string
		after time.Time
		want  time.Time
		never bool
	}{
		{
			doc:  "daily before the hour",
			expr: "0 3 * * *", after: utc(2026, time.January, 15, 2, 0),
			want: utc(2026, time.January, 15, 3, 0),
		},
		{
			doc:  "strictly after an exact hit",
			expr: "0 3 * * *", after: utc(2026, time.January, 15, 3, 0),
			want: utc(2026, time.January, 16, 3, 0),
		},
		{
			doc:  "seconds are truncated away",
			expr: "0 3 * * *", after: utc(2026, time.January, 15, 2, 59).Add(30 * time.Second),
			want: utc(2026, time.January, 15, 3, 0),
		},
		{
			doc:  "quarter-hour step",
			expr: "*/15 * * * *", after: utc(2026, time.January, 15, 10, 7),
			want: utc(2026, time.January, 15, 10, 15),
		},
		{
			doc:  "step wraps to next hour",
			expr: "*/15 * * * *", after: utc(2026, time.January, 15, 10, 45),
			want: utc(2026, time.January, 15, 11, 0),
		},
		{
			doc:  "yearly",
			expr: "0 0 1 1 *", after: utc(2026, time.June, 1, 0, 0),
			want: utc(2027, time.January, 1, 0, 0),
		},
		{
			doc:  "weekday match",
			expr: "0 12 * * 0", after: utc(2026, time.January, 15, 0, 0), // a Thursday
			want: utc(2026, time.January, 18, 12, 0), // the next Sunday
		},
		{
			doc:  "sunday spelled as 7",
			expr: "0 12 * * 7", after: utc(2026, time.January, 15, 0, 0),
			want: utc(2026, time.January, 18, 12, 0),
		},
		{
			doc: "restricted day-of-month and day-of-week combine as OR",
			// The next Friday (March 6th) comes before the next 13th.
			expr: "0 0 13 * 5", after: utc(2026, time.March, 1, 0, 0),
			want: utc(2026, time.March, 6, 0, 0),
		},
		{
			doc: "star-step day-of-month does not defeat the weekday",
			// Vixie's first-character rule: */1 counts as unrestricted, so
			// the weekday alone constrains the day.
			expr: "0 3 */1 * 1", after: utc(2026, time.January, 15, 0, 0), // a Thursday
			want: utc(2026, time.January, 19, 3, 0), // the next Monday
		},
		{
			doc: "star-step day-of-month mask still applies",
			// January 16th is even: the next odd day is the 17th.
			expr: "0 3 */2 * *", after: utc(2026, time.January, 15, 4, 0),
			want: utc(2026, time.January, 17, 3, 0),
		},
		{
			doc:  "february 29th waits for a leap year",
			expr: "0 0 29 2 *", after: utc(2026, time.January, 1, 0, 0),
			want: utc(2028, time.February, 29, 0, 0),
		},
		{
			doc: "sparse weekday-bound february 29th",
			// February 29th on a Sunday: after 2032 the next is 2060.
			expr: "0 0 29 2 */7", after: utc(2032, time.March, 1, 0, 0),
			want: utc(2060, time.February, 29, 0, 0),
		},
		{
			doc:  "impossible date never fires",
			expr: "0 0 30 2 *", after: utc(2026, time.January, 1, 0, 0),
			never: true,
		},
	} {
		t.Run(tc.doc, func(t *testing.T) {
			schedule, err := parseCron(tc.expr)
			assert.NilError(t, err)
			got, ok := schedule.next(tc.after, time.UTC)
			if tc.never {
				assert.Check(t, !ok)
				return
			}
			assert.Assert(t, ok)
			assert.Check(t, is.Equal(got.String(), tc.want.String()))
		})
	}
}

func TestCronNextDST(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	assert.NilError(t, err)
	schedule, err := parseCron("30 2 * * *")
	assert.NilError(t, err)

	t.Run("spring forward skips the nonexistent hour", func(t *testing.T) {
		// On 2026-03-29 the Paris wall clock jumps from 02:00 to 03:00;
		// 02:30 never happens that day.
		after := time.Date(2026, time.March, 29, 0, 0, 0, 0, paris)
		got, ok := schedule.next(after, paris)
		assert.Assert(t, ok)
		assert.Check(t, is.Equal(got.String(), time.Date(2026, time.March, 30, 2, 30, 0, 0, paris).String()))
	})

	t.Run("fall back fires once in the repeated hour", func(t *testing.T) {
		// On 2026-10-25 the Paris wall clock falls from 03:00 back to
		// 02:00: 02:30 happens twice (CEST then CET). The occurrence fires
		// on the first one only.
		after := time.Date(2026, time.October, 25, 0, 0, 0, 0, paris)
		first, ok := schedule.next(after, paris)
		assert.Assert(t, ok)
		// The first 02:30 is the CEST one, i.e. 00:30 UTC.
		assert.Check(t, is.Equal(first.UTC().String(), time.Date(2026, time.October, 25, 0, 30, 0, 0, time.UTC).String()))

		// The occurrence after it is the next day's, not the repeated
		// (CET, 01:30 UTC) 02:30 of the same day.
		second, ok := schedule.next(first, paris)
		assert.Assert(t, ok)
		assert.Check(t, is.Equal(second.String(), time.Date(2026, time.October, 26, 2, 30, 0, 0, paris).String()))
	})

	t.Run("fall back does not repeat the hour after a fire on its last minute", func(t *testing.T) {
		// The occurrence after 02:59 CEST is a minute later in real time,
		// at 02:00 CET, but that wall-clock time already fired an hour ago.
		schedule, err := parseCron("0,59 2 * * *")
		assert.NilError(t, err)

		lastMinute := time.Date(2026, time.October, 25, 0, 59, 0, 0, time.UTC)
		got, ok := schedule.next(lastMinute, paris)
		assert.Assert(t, ok)
		assert.Check(t, is.Equal(got.String(), time.Date(2026, time.October, 26, 2, 0, 0, 0, paris).String()))
	})

	t.Run("transition skipping midnight does not stall the day search", func(t *testing.T) {
		// On 2026-03-08 the Havana wall clock jumps from 00:00 to 01:00, so
		// that day's midnight normalizes back into March 7.
		havana, err := time.LoadLocation("America/Havana")
		assert.NilError(t, err)
		schedule, err := parseCron("0 0 * * 1")
		assert.NilError(t, err)

		after := time.Date(2026, time.March, 7, 12, 0, 0, 0, havana) // a Saturday
		got, ok := schedule.next(after, havana)
		assert.Assert(t, ok)
		assert.Check(t, is.Equal(got.String(), time.Date(2026, time.March, 9, 0, 0, 0, 0, havana).String()))
	})
}
