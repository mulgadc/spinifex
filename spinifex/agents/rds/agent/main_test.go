package agent

import (
	"os"
	"testing"
	"time"
)

// TestMain shortens the boot-retry backoff for the whole package: the tests
// assert which calls are retried, not how long the agent waits between them.
func TestMain(m *testing.M) {
	retryMin, retryMax = time.Millisecond, time.Millisecond
	os.Exit(m.Run())
}
