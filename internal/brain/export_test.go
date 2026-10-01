package brain

import "time"

// SetNow replaces the clock request assembly reads, so a test can assemble a
// request on a day of its choosing.
func (b *Brain) SetNow(now func() time.Time) { b.now = now }
