package image

import (
	"os"
	"testing"
	"time"

	ec2snapshot "github.com/mulgadc/spinifex/spinifex/domains/ec2/snapshot"
)

// TestMain lowers the snapshot claim retry delay, so a test that waits out a
// held claim takes milliseconds, not two seconds.
func TestMain(m *testing.M) {
	ec2snapshot.ClaimRetryDelay = time.Millisecond //nolint:reassign // set once before any test builds a service
	os.Exit(m.Run())
}
