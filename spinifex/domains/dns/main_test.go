package dns

import (
	"os"
	"testing"
	"time"
)

// TestMain shortens the retry and poll waits once, before any test runs, so
// the tests that wait on them can run in parallel without a data race.
func TestMain(m *testing.M) {
	bootstrapRetryDelay = time.Millisecond
	delegateLockPoll = 10 * time.Millisecond
	os.Exit(m.Run())
}
