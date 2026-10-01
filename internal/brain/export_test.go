package brain

import "time"

// SetNow replaces the clock request assembly reads, so a test can assemble a
// request on a day of its choosing.
func (b *Brain) SetNow(now func() time.Time) { b.now = now }

// Now reads the clock request assembly reads, so a test can see the one New
// wires in.
func (b *Brain) Now() time.Time { return b.now() }
