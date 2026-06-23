//go:build e2e

package single

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// runPredastoreObjectLifecycle drives predastore's user-visible object contract
// (write → read → delete → gone → still-serving) against the deployed S3
// endpoint. Predastore shares the host the AWS gateway binds, so resolve it the
// same way harness.NewAWSClient does.
func runPredastoreObjectLifecycle(t *testing.T, fix *Fixture) {
	host := "127.0.0.1"
	if len(fix.Env.ServiceIPs) > 0 {
		host = fix.Env.ServiceIPs[0]
	}
	harness.AssertPredastoreObjectLifecycle(context.Background(), t, host)
}
