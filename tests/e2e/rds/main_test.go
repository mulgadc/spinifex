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
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
)

// Mirrors the AWS client's own default, since the endpoint name the control
// plane publishes is region-qualified.
const defaultRegion = "ap-southeast-2"

// The PostgreSQL instance spec most of the suite creates from; the MariaDB
// cases carry their own engine, database name and credentials and share the
// class and storage. The class is the floor and the storage is the API's own
// minimum: nothing here needs more, and each DB VM booted is charged against the
// phase's instance budget. Only a class-sensitive assertion names a bigger
// class, and only a grow names a bigger size.
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

// The suite's whole guest-memory allowance on the node, in GiB. Each DB instance
// is a guest with its own data volume on a node that is also running the cluster,
// and the phase budget — 25 minutes wall clock — is written against four floor
// instances alive at once.
//
// Denominated in memory rather than instances because the node admits a launch
// on live MemAvailable: a budget counting VMs lets a test holding two db.t3.small
// through a cap written for four db.t3.micro, and the launch is then refused with
// InsufficientInstanceCapacity rather than made to wait.
const totalVMBudgetGiB = 4

// The two nano client VMs, which come out of the same node memory but no test's
// reservation. Deducted up front rather than acquired: a test asking for one
// while holding its own DB reservation would deadlock against itself.
const clientVMBudgetGiB = 1

// What is left for DB instances. Every instance-owning test runs in parallel and
// takes its allowance from here.
const maxConcurrentDBVMGiB = totalVMBudgetGiB - clientVMBudgetGiB

// What each class the suite names costs against that budget.
var dbClassGiB = map[string]int64{
	dbClass:    1,
	grownClass: 2,
}

var (
	pkgFixOnce sync.Once
	pkgFix     *Fixture
	pkgFixErr  error

	dbVMSlots = semaphore.NewWeighted(maxConcurrentDBVMGiB)
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
	// How many tests may run at once is dbVMSlots' business, not the runner's CPU
	// count: -test.parallel defaults to GOMAXPROCS, so a two-core CI VM would
	// quietly halve the concurrency this suite's budget is written against. The
	// budget in GiB is the right ceiling because no test reserves less than a
	// floor instance. An explicit -test.parallel on the command line still wins,
	// since flag parsing happens inside m.Run.
	if err := flag.Set("test.parallel", strconv.Itoa(maxConcurrentDBVMGiB)); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: set test.parallel: %v\n", err)
		os.Exit(1)
	}

	start := time.Now()
	code := m.Run()
	reportCreateLatencies(time.Since(start))
	if pkgFix != nil && pkgFix.Harness != nil {
		if err := pkgFix.Harness.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e teardown: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// reserveDBVMs takes a test's whole DB-VM allowance in one acquisition, before
// it creates anything. classes is the test's peak — the class of every instance
// it will have alive at once — so a test that creates and deletes one instance at
// a time names one however many it gets through, and one whose instance ends up
// on a bigger class names the class it ends on.
//
// The acquisition is atomic because acquiring per instance deadlocks: four tests
// each holding a floor instance's worth and each waiting for a second would never
// progress.
func reserveDBVMs(t *testing.T, classes ...string) {
	t.Helper()
	var gib int64
	for _, class := range classes {
		cost, known := dbClassGiB[class]
		require.True(t, known, "class %s has no entry in dbClassGiB", class)
		gib += cost
	}
	require.LessOrEqual(t, gib, int64(maxConcurrentDBVMGiB),
		"a test cannot reserve more DB-VM memory than the whole suite is allowed")

	start := time.Now()
	if err := dbVMSlots.Acquire(context.Background(), gib); err != nil {
		t.Fatalf("reserve %d GiB of DB-VM budget: %v", gib, err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Logf("waited %s for %d of %d GiB of DB-VM budget",
			waited.Round(time.Second), gib, maxConcurrentDBVMGiB)
	}

	// Registered before any instance's teardown, so LIFO hands the budget back
	// only once the last DB VM this test owns is actually gone.
	t.Cleanup(func() { dbVMSlots.Release(gib) })
}

// Subtests report under their parent, so a create issued from one is attributed
// to the top-level test that reserved the slot it is using.
func topLevelName(t *testing.T) string {
	name, _, _ := strings.Cut(t.Name(), "/")
	return name
}

var (
	clientVMMu sync.Mutex
	clientVM   harness.SSHTarget
)

// rdsClient returns the shared client VM, built under a package lock. The
// instance itself is memoised by the process fixture, but the keypair, default
// VPC and security-group rules RDSClientVM readies first are not safe to drive
// concurrently — and the tests that need a client now run in parallel.
func rdsClient(t *testing.T, f *Fixture) harness.SSHTarget {
	t.Helper()
	clientVMMu.Lock()
	defer clientVMMu.Unlock()
	if clientVM.Host == "" {
		clientVM = harness.RDSClientVM(t, f.AWS, f.Harness, f.Env)
	}
	return clientVM
}

var (
	latencyMu         sync.Mutex
	dbCreateStarted   = map[string]time.Time{}
	dbCreateLatencies []dbCreateLatency
)

type dbCreateLatency struct {
	test string
	id   string
	took time.Duration
}

// waitForAvailable waits for a DB instance and, the first time it does so for a
// freshly created one, records how long create → available took. That boot is
// the suite's dominant cost and the number the phase budget is written against,
// so it is reported per instance rather than inferred from the run's total.
func waitForAvailable(t *testing.T, f *Fixture, id string) *rds.DBInstance {
	t.Helper()
	instance := harness.WaitForDBInstanceAvailable(t, f.AWS, id)

	latencyMu.Lock()
	started, first := dbCreateStarted[id]
	took := time.Since(started)
	if first {
		delete(dbCreateStarted, id)
		dbCreateLatencies = append(dbCreateLatencies,
			dbCreateLatency{test: topLevelName(t), id: id, took: took})
	}
	latencyMu.Unlock()

	if first {
		t.Logf("%s reached available %s after its create was issued", id, took.Round(time.Second))
	}
	return instance
}

// reportCreateLatencies prints what every DB VM in the run cost, slowest first.
// A suite that drifts past its budget should say which create got slower.
func reportCreateLatencies(wall time.Duration) {
	latencyMu.Lock()
	defer latencyMu.Unlock()
	if len(dbCreateLatencies) == 0 {
		return
	}
	sort.Slice(dbCreateLatencies, func(i, j int) bool {
		return dbCreateLatencies[i].took > dbCreateLatencies[j].took
	})
	fmt.Fprintf(os.Stderr, "\nRDS suite: %s wall clock, %d DB instances, ≤%d GiB concurrent\n",
		wall.Round(time.Second), len(dbCreateLatencies), maxConcurrentDBVMGiB)
	for _, l := range dbCreateLatencies {
		fmt.Fprintf(os.Stderr, "  create→available %8s  %s (%s)\n", l.took.Round(time.Second), l.id, l.test)
	}
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
func createDBInstance(t *testing.T, f *Fixture, id string, opts ...func(*rds.CreateDBInstanceInput)) *rds.DBInstance {
	t.Helper()
	return createDBInstanceFrom(t, f, validCreateInput(id), opts...)
}

// createDBInstanceFrom is createDBInstance for a request the caller assembled
// itself, which is what the MariaDB cases need: their engine, database name and
// credentials are not the suite's PostgreSQL defaults.
//
// Every test that owns an instance bottoms out here, because the three things a
// test that boots a DB VM must not forget are registered here: the teardown, so
// a failed run does not charge the next one, the failure-only diagnostic bundle,
// without which "create timed out" names no owning phase, and the charge against
// the test's DB-VM reservation.
func createDBInstanceFrom(t *testing.T, f *Fixture, in *rds.CreateDBInstanceInput,
	opts ...func(*rds.CreateDBInstanceInput)) *rds.DBInstance {
	t.Helper()
	for _, opt := range opts {
		opt(in)
	}
	id := aws.StringValue(in.DBInstanceIdentifier)
	markDBCreateStarted(id)
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

// Stamps the create so waitForAvailable can report what the boot cost.
func markDBCreateStarted(id string) {
	latencyMu.Lock()
	defer latencyMu.Unlock()
	dbCreateStarted[id] = time.Now()
}
