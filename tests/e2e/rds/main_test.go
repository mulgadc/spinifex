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
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
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
	pkgFixErr  error
)

// Fixture carries per-process state shared across every Test* in this package.
type Fixture struct {
	Env        *harness.Env
	AWS        *harness.AWSClient
	Account    string
	Region     string
	BaseDomain string

	// The Ensure* fixture the client VM and its keypair hang off. Process-scoped
	// rather than bound to a test: the client guest is shared by every test that
	// needs a connection, and must outlive whichever one built it.
	Harness *harness.Fixture

	// The system-account client, built on first use: it shells to sudo, and only
	// the tests that reach behind a DB instance need one.
	systemOnce sync.Once
	system     *harness.AWSClient
}

// TestMain drains the process fixture's cleanup chain after the run, so the
// client VM and its keypair are reclaimed whichever test built them. A leaked
// resource fails the run: the suite may have passed, but it left state behind
// that the next run trips over.
func TestMain(m *testing.M) {
	code := m.Run()
	if pkgFix != nil && pkgFix.Harness != nil {
		if err := pkgFix.Harness.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
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
		h, err := harness.NewProcessFixture(awsCli)
		if err != nil {
			pkgFixErr = err
			return
		}
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
			Harness:    h,
		}
	})
	if pkgFixErr != nil {
		t.Fatalf("rds fixture init failed: %v", pkgFixErr)
	}
	if pkgFix == nil {
		t.Skip("SPINIFEX_E2E is unset")
	}
	return pkgFix
}

// SystemAWS returns the system-account client. The DB VM and its data volume
// belong to that account and are filtered out of the suite's own describes, so
// every assertion behind a DB instance goes through here.
func (f *Fixture) SystemAWS(t *testing.T) *harness.AWSClient {
	t.Helper()
	f.systemOnce.Do(func() { f.system = harness.SystemAWSClient(t, f.Env) })
	return f.system
}

// The clients and output directory a DB-instance diagnostic bundle needs.
func (f *Fixture) dbDiag(t *testing.T) harness.DBDiag {
	t.Helper()
	return harness.DBDiag{Tenant: f.AWS, System: f.SystemAWS(t), Dir: harness.ArtifactDir(t, f.Env)}
}

// The suite's own create request: valid as it stands, so a caller mutates only
// the field it cares about.
func validCreateInput(id string) *rds.CreateDBInstanceInput {
	return &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(id),
		Engine:               aws.String(dbEngine),
		DBInstanceClass:      aws.String(dbClass),
		AllocatedStorage:     aws.Int64(dbStorageGiB),
		DBName:               aws.String(dbName),
		MasterUsername:       aws.String(dbMasterUser),
		MasterUserPassword:   aws.String(dbMasterPassword),
	}
}

// createDBInstance creates the suite's standard instance and returns the create
// response's own view of it — status creating, no endpoint yet.
//
// Every test that owns an instance goes through here, because the two things a
// test that boots a DB VM must not forget are registered here: the teardown, so
// a failed run does not charge the next one, and the failure-only diagnostic
// bundle, without which "create timed out" names no owning phase.
func createDBInstance(t *testing.T, f *Fixture, id string, opts ...func(*rds.CreateDBInstanceInput)) *rds.DBInstance {
	t.Helper()
	in := validCreateInput(id)
	for _, opt := range opts {
		opt(in)
	}
	out, err := f.AWS.RDS.CreateDBInstance(in) //nolint:staticcheck // e2e:allow-create — the instance under test
	require.NoError(t, err, "create-db-instance %s", id)
	require.NotNil(t, out.DBInstance)
	t.Cleanup(func() { deleteInstance(t, f, id) })
	harness.CaptureDBDiagnostics(t, f.dbDiag(t), id)
	return out.DBInstance
}

// A create that must be refused, made with whichever principal the assertion is
// about. Deletes whatever it created if it was not refused: a create nobody
// expected to succeed is otherwise a DB VM nobody waits for and nobody tears down.
func expectCreateRefused(t *testing.T, f *Fixture, c *harness.AWSClient, code string, in *rds.CreateDBInstanceInput) {
	t.Helper()
	out, err := c.RDS.CreateDBInstance(in) //nolint:staticcheck // e2e:allow-create — asserted to be refused
	if err == nil {
		id := aws.StringValue(out.DBInstance.DBInstanceIdentifier)
		deleteInstance(t, f, id)
		t.Fatalf("create of %s was accepted; expected %s", id, code)
	}
	harness.AssertAWSError(t, err, code)
}

// Teardown for one instance: idempotent, and waits for the record to go so a
// group or a snapshot the next step deletes is no longer held.
func deleteInstance(t *testing.T, f *Fixture, id string) {
	t.Helper()
	deleteInstanceAs(t, f.AWS, id)
}

// The same teardown for an instance another tenant owns, which only that
// tenant's credentials can see at all.
func deleteInstanceAs(t *testing.T, c *harness.AWSClient, id string) {
	t.Helper()
	_, err := c.RDS.DeleteDBInstance(&rds.DeleteDBInstanceInput{
		DBInstanceIdentifier: aws.String(id),
		SkipFinalSnapshot:    aws.Bool(true),
	})
	if err != nil {
		if !harness.ErrorCodeIs(err, "DBInstanceNotFound") {
			t.Logf("delete-db-instance %s: %v (left behind for manual teardown)", id, err)
		}
		return
	}
	harness.WaitForDBInstanceGone(t, c, id)
}
