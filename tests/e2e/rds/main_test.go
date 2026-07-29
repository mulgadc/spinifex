//go:build e2e

// Package rds is the RDS E2E suite: the create/describe/connect path against a
// running cluster. The control-plane orchestration, the validation matrix and
// the reconciler's status transitions are covered by the handlers/rds unit
// tests; what only a live cluster can prove is that a DB VM actually boots, the
// in-guest agent reports healthy, the endpoint resolves and a client can speak
// the wire protocol to it.
//
// The suite is opt-in beyond SPINIFEX_E2E: DeleteDBInstance is not implemented
// yet, so a run leaves its DB instance and VM behind. Set SPINIFEX_E2E_RDS=1 to
// accept that on a cluster you can rebuild.
package rds

import (
	"os"
	"sync"
	"testing"

	"github.com/mulgadc/spinifex/tests/e2e/harness"
)

// Mirrors the AWS client's own default, since the endpoint name the control
// plane publishes is region-qualified.
const defaultRegion = "ap-southeast-2"

var (
	pkgFixOnce sync.Once
	pkgFix     *Fixture
)

// Fixture carries per-process state shared across every Test* in this package.
type Fixture struct {
	Env        *harness.Env
	AWS        *harness.AWSClient
	Account    string
	Region     string
	BaseDomain string
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// requireRDSFixture returns the package-scoped Fixture, building it on first
// call. Skips the calling test unless both gates are set.
func requireRDSFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" || os.Getenv("SPINIFEX_E2E_RDS") != "1" {
			return
		}
		env := harness.LoadEnv(t)
		awsCli := harness.NewAWSClient(t, env)
		region := os.Getenv("SPINIFEX_AWS_REGION")
		if region == "" {
			region = defaultRegion
		}
		pkgFix = &Fixture{
			Env:        env,
			AWS:        awsCli,
			Account:    harness.IAMAccountID(t, awsCli),
			Region:     region,
			BaseDomain: harness.NorthstarBaseDomain(env),
		}
	})
	if pkgFix == nil {
		t.Skip("rds fixture unavailable (SPINIFEX_E2E unset, or SPINIFEX_E2E_RDS != 1 — the suite leaks a DB VM until DeleteDBInstance lands)")
	}
	return pkgFix
}
