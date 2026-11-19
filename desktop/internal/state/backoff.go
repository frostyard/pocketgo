package state

import "time"

// Backoff is how long the event stream waits before its next try after it
// drops: Min after the first failure in a row, doubling with each further
// failure, never more than Max. A stream that opens (the service answers
// GET /v1/events with 200) ends the run of failures.
type Backoff struct {
	Min, Max time.Duration
}

// DefaultBackoff tries again after 1 s, then 2, 4, 8 and 16 s, then every
// 30 s.
var DefaultBackoff = Backoff{Min: time.Second, Max: 30 * time.Second}

// Delay is the wait after failures failures in a row; 0 for none.
func (b Backoff) Delay(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	if b.Min <= 0 {
		b = DefaultBackoff
	}
	if b.Max < b.Min {
		b.Max = b.Min
	}
	d := b.Min
	for i := 1; i < failures && d < b.Max; i++ {
		d *= 2
	}
	return min(d, b.Max)
}
