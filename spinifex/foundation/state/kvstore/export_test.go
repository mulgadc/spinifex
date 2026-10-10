package kvstore

import "time"

// SetOpenRetryInterval shortens the OpenWithRetry pause for tests and returns a
// function restoring the previous value.
func SetOpenRetryInterval(d time.Duration) func() {
	return swapOpenRetryInterval(d)
}
