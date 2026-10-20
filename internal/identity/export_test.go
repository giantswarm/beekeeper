package identity

import "time"

// SetNow replaces the exchanger's clock.
func (e *Exchanger) SetNow(now func() time.Time) { e.now = now }
