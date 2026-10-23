package clock

import "time"

// System is the production clock. Everything is UTC.
type System struct{}

func (System) Now() time.Time { return time.Now().UTC() }
