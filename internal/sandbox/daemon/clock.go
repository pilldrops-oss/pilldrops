package daemon

import "time"

// Clock abstracts time so the reconciler and meter are deterministically testable.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// manualClock is a test clock advanced by hand.
type manualClock struct{ t time.Time }

func (c *manualClock) Now() time.Time          { return c.t }
func (c *manualClock) Advance(d time.Duration) { c.t = c.t.Add(d) }
