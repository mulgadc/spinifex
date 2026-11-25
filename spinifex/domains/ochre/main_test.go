package ochre

import (
	"os"
	"testing"
	"time"
)

// TestMain shortens the embedder's transport-retry pause, which no test
// asserts on, so tests that exhaust the retries do not wait it out.
func TestMain(m *testing.M) {
	embedTransportRetryBackoff = time.Millisecond
	os.Exit(m.Run())
}
