//go:build e2e

// Package rds is the RDS E2E suite: the create/describe/connect path against a
// running cluster. The control-plane orchestration, the validation matrix and
// the reconciler's status transitions are covered by the handlers/rds unit
// tests; what only a live cluster can prove is that a DB VM actually boots, the
// in-guest agent reports healthy, the endpoint resolves and a client can speak
// the wire protocol to it.
//
// Gated on SPINIFEX_E2E alone: every test here deletes the instances it creates,
// so there is nothing left for an operator to accept.
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

// The instance spec every test in the suite creates from. The class is the D15
// floor and the storage is the API's own minimum: nothing here needs more, and
// each DB VM booted is charged against the phase's instance budget. Only a
// class-sensitive assertion names a bigger class, and only a grow names a
// bigger size.
const (
	dbInstancePfx = "rds-e2e"
	dbEngine      = "postgres"
	dbClass       = "db.t3.micro"
	dbStorageGiB  = 20
	dbName        = "orders"
	dbMasterUser  = "appuser"
	// No '/', '"', '@' or spaces: the characters the API rejects because they
	// break a connection string or the engine's own role syntax.
	dbMasterPassword = "e2eSup3rSecret1"
)

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
// call. Skips the calling test when the suite's gate is unset.
func requireRDSFixture(t *testing.T) *Fixture {
	t.Helper()
	pkgFixOnce.Do(func() {
		if os.Getenv("SPINIFEX_E2E") == "" {
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
		t.Skip("SPINIFEX_E2E is unset")
	}
	return pkgFix
}
