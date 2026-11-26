package message

import "time"

// SetWaitUnit sets the length of one wait second for r.
func SetWaitUnit(r *Receiver, d time.Duration) { r.waitUnit = d }
