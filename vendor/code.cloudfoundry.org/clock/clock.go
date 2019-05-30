package clock

import "time"

type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
	Since(t time.Time) time.Duration
	// After waits for the duration to elapse and then sends the current time
	// on the returned channel.
	// It is equivalent to clock.NewTimer(d).C.
	// The underlying Timer is not recovered by the garbage collector
	// until the timer fires. If efficiency is a concern, use clock.NewTimer
	// instead and call Timer.Stop if the timer is no longer needed.
	After(d time.Duration) <-chan time.Time

	NewTimer(d time.Duration) Timer
	NewTicker(d time.Duration) Ticker
}

type realClock struct{}

func NewClock() Clock {
	return &realClock{}
}

func (clock *realClock) Now() time.Time {
	return time.Now()
}

func (clock *realClock) Since(t time.Time) time.Duration {
	return time.Now().Sub(t)
}

func (clock *realClock) Sleep(d time.Duration) {
	<-clock.NewTimer(d).C()
}

func (clock *realClock) After(d time.Duration) <-chan time.Time {
	return clock.NewTimer(d).C()
}

func (clock *realClock) NewTimer(d time.Duration) Timer {
	return &realTimer{
		t: time.NewTimer(d),
	}
}

func (clock *realClock) NewTicker(d time.Duration) Ticker {
	return &realTicker{
		t: time.NewTicker(d),
	}
}
