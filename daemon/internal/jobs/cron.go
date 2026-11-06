package jobs

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	// Schedules name IANA timezones and must evaluate the same on every
	// host, including Windows and minimal images without zoneinfo. The
	// system database still takes precedence where it exists.
	_ "time/tzdata"
)

// cronSchedule is a parsed five-field crontab expression. Each set is a
// bitmask of the accepted values for its field.
type cronSchedule struct {
	minutes uint64 // 0-59
	hours   uint64 // 0-23
	dom     uint64 // 1-31, day of month
	months  uint64 // 1-12
	dow     uint64 // 0-6, day of week, Sunday is 0

	// The day-of-month and day-of-week fields follow the standard crontab
	// quirk: when both are restricted (neither is *), a day matching either
	// fires; otherwise the restricted one must match.
	domRestricted, dowRestricted bool
}

// nextCronBoundYears caps the search for the next occurrence. The Gregorian
// calendar, weekdays included, repeats every 400 years, so an expression
// with no occurrence in that span never fires, as with impossible dates
// such as "0 0 30 2 *"; sparse ones such as "0 0 29 2 */7" (February 29 on
// a Sunday) still find occurrences decades apart. The scan jumps over
// unmatched months and days, so even a full-span search stays in the
// milliseconds.
const nextCronBoundYears = 400

// parseCron parses a five-field crontab expression: minute, hour,
// day-of-month, month, day-of-week. Each field accepts numbers, lists
// (a,b,c), ranges (a-b) and steps (*/n or a-b/n).
//
// The wire format is deliberately strict: month and weekday names, @daily
// style shortcuts, and Quartz extensions (L, #, W) are rejected. Clients
// expand shortcuts before submitting, so the daemon-side grammar can stay
// small and unambiguous.
func parseCron(expr string) (*cronSchedule, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression must have 5 fields (minute hour day-of-month month day-of-week), got %d", len(fields))
	}
	var (
		schedule cronSchedule
		err      error
	)
	if schedule.minutes, err = parseCronField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute field: %w", err)
	}
	if schedule.hours, err = parseCronField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour field: %w", err)
	}
	if schedule.dom, err = parseCronField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day-of-month field: %w", err)
	}
	if schedule.months, err = parseCronField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month field: %w", err)
	}
	if schedule.dow, err = parseCronField(fields[4], 0, 7); err != nil {
		return nil, fmt.Errorf("day-of-week field: %w", err)
	}
	// Both 0 and 7 mean Sunday; fold 7 onto 0 so matching only checks 0-6.
	if schedule.dow&(1<<7) != 0 {
		schedule.dow = (schedule.dow &^ (1 << 7)) | 1
	}
	// Vixie's first-character rule: a field is "restricted" only when it
	// does not start with *, so */2 counts as unrestricted for the AND/OR
	// day-combination choice while its mask still applies.
	schedule.domRestricted = !strings.HasPrefix(fields[2], "*")
	schedule.dowRestricted = !strings.HasPrefix(fields[4], "*")
	return &schedule, nil
}

// parseCronField parses one field into a bitmask of accepted values.
func parseCronField(field string, minValue, maxValue int) (uint64, error) {
	var mask uint64
	for part := range strings.SplitSeq(field, ",") {
		rangeExpr, stepExpr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			// Steps only make sense over a span; a step on a single value
			// ("3/5") is a Vixie laxity this grammar deliberately rejects.
			if rangeExpr != "*" && !strings.Contains(rangeExpr, "-") {
				return 0, fmt.Errorf("invalid term %q: a step requires * or a range", part)
			}
			// Atoi tolerates a leading sign; the grammar is digits only.
			if stepExpr == "" || stepExpr[0] < '0' || stepExpr[0] > '9' {
				return 0, fmt.Errorf("invalid step in term %q", part)
			}
			parsed, err := strconv.Atoi(stepExpr)
			if err != nil || parsed <= 0 {
				return 0, fmt.Errorf("invalid step in term %q", part)
			}
			step = parsed
		}

		lo, hi := minValue, maxValue
		if rangeExpr != "*" {
			loExpr, hiExpr, isRange := strings.Cut(rangeExpr, "-")
			var err error
			if lo, err = parseCronValue(loExpr, minValue, maxValue); err != nil {
				return 0, fmt.Errorf("invalid term %q: %w", part, err)
			}
			hi = lo
			if isRange {
				if hi, err = parseCronValue(hiExpr, minValue, maxValue); err != nil {
					return 0, fmt.Errorf("invalid term %q: %w", part, err)
				}
				if hi < lo {
					return 0, fmt.Errorf("invalid term %q: range is reversed", part)
				}
			}
		}
		for v := lo; v <= hi; v += step {
			mask |= 1 << v
			// Check the remaining span before adding an untrusted step,
			// which may be large enough to overflow an int.
			if step > hi-v {
				break
			}
		}
	}
	if mask == 0 {
		return 0, errors.New("field is empty")
	}
	return mask, nil
}

// parseCronValue parses one numeric field value and checks its bounds.
func parseCronValue(expr string, minValue, maxValue int) (int, error) {
	// Atoi tolerates a leading sign; the grammar is digits only.
	if expr == "" || expr[0] < '0' || expr[0] > '9' {
		return 0, fmt.Errorf("%q is not a number (names and shortcuts are not supported)", expr)
	}
	value, err := strconv.Atoi(expr)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number (names and shortcuts are not supported)", expr)
	}
	if value < minValue || value > maxValue {
		return 0, fmt.Errorf("value %d is outside %d-%d", value, minValue, maxValue)
	}
	return value, nil
}

// next returns the first occurrence strictly after the given time, evaluated
// on the wall clock of loc, and false when no occurrence exists within the
// search bound.
//
// Daylight-saving transitions follow wall-clock semantics: an occurrence
// falling in the skipped hour of a spring-forward day does not fire that day
// (the wall-clock time never happens), and an occurrence in the repeated
// hour of a fall-back day fires once, at the first occurrence.
func (s *cronSchedule) next(after time.Time, loc *time.Location) (time.Time, bool) {
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)

	bound := after.AddDate(nextCronBoundYears, 0, 0)
	for t.Before(bound) {
		if s.months&(1<<uint(t.Month())) == 0 {
			// Jump to the first minute of the next month.
			t = jumpForward(t, time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc))
			continue
		}
		if !s.dayMatches(t) {
			t = jumpForward(t, time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc))
			continue
		}
		if s.hours&(1<<uint(t.Hour())) == 0 {
			// Advance in absolute time rather than by rebuilding wall-clock
			// components: across a fall-back transition the rebuilt time is
			// ambiguous and Go may resolve it to the second occurrence,
			// skipping the first one; the absolute walk visits both in real
			// order and the fields are re-checked on every loop anyway.
			t = t.Add(time.Duration(60-t.Minute()) * time.Minute)
			continue
		}
		if s.minutes&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		// The first pass through a fall-back day's repeated hour already
		// matched this wall-clock time, so it fires only once.
		if end, repeated := repeatedWallClock(t); repeated {
			t = end
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

// jumpForward returns target, or the minute after t when target is not after
// it. Where a transition skips midnight, as in America/Havana, time.Date
// normalizes a day's midnight back into the previous day, so jumping to it
// would repeat forever; walking minutes reaches the transition instead.
func jumpForward(t, target time.Time) time.Time {
	if target.After(t) {
		return target
	}

	return t.Add(time.Minute)
}

// repeatedWallClock reports whether t's wall-clock time already happened
// earlier, because a transition just set the clock back, and returns when
// the repeated span ends.
func repeatedWallClock(t time.Time) (end time.Time, repeated bool) {
	start, _ := t.ZoneBounds()
	if start.IsZero() {
		return time.Time{}, false
	}

	_, offset := t.Zone()
	_, before := start.Add(-time.Second).Zone()

	setBack := time.Duration(before-offset) * time.Second
	if setBack <= 0 || t.Sub(start) >= setBack {
		return time.Time{}, false
	}

	return start.Add(setBack), true
}

// dayMatches applies Vixie cron's day-combination rule: both masks always
// apply, combined with OR only when both fields are restricted ("1,15 * Sun"
// means the 1st, the 15th AND Sundays) and with AND otherwise (an
// unrestricted field's mask is full, so it never excludes anything).
func (s *cronSchedule) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	if s.domRestricted && s.dowRestricted {
		return domOK || dowOK
	}
	return domOK && dowOK
}
