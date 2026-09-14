//go:build e2e

package rds

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The scale factor to build the database at. pgbench costs roughly 16 MiB
	// per unit, so this puts the data comfortably past the 256 MiB of shared
	// buffers a floor instance gets — the pool is then full of pages the engine
	// has to write out on the way down, rather than a dataset that fits.
	gracefulRebootScale = 25

	// How long the read-write run drives the instance for, to leave the buffer
	// pool dirty at the moment the reboot arrives.
	gracefulRebootDirtyRun = "45"

	// The probe table and how many rows go in it.
	gracefulRebootTable = "e2e_graceful_reboot_probe"
	gracefulRebootRows  = 1000
)

// TestLoadedGracefulReboot reboots a loaded PostgreSQL instance and asserts the
// engine was asked to shut down rather than reset under.
//
// The probe is an UNLOGGED table, and the choice is the whole test. PostgreSQL
// writes no WAL for one, so its contents are durable exactly when the engine
// shuts down cleanly: a clean shutdown checkpoints them, and crash recovery
// truncates every unlogged relation unconditionally. The row count afterwards is
// therefore a direct read of which of the two happened.
//
// The obvious assertions do not work. Rows committed normally survive a hard
// reset too — that is what the WAL is for — so a test that writes, resets and
// finds its data would pass against a reset that lost the guest's entire page
// cache. `synchronous_commit = off` does expose the gap, but only for the few
// hundred milliseconds before the WAL writer flushes, so whether it catches a
// hard reset depends on how fast the reset arrives. An unlogged table has no
// such window.
func TestLoadedGracefulReboot(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	reserveDBVMs(t, dbClass)

	id := fmt.Sprintf("%s-gracereboot-%d", dbInstancePfx, time.Now().Unix())

	harness.Phase(t, "Creating DB instance %q", id)
	createDBInstance(t, f, id)
	client := rdsClient(t, f)

	instance := waitForAvailable(t, f, id)
	conn := harness.PSQLConnFor(t, instance, dbMasterUser, dbMasterPassword, dbName)

	harness.Phase(t, "Loading %q past its shared buffers", id)
	sharedBuffers := queryBytes(t, client, conn,
		"SELECT setting::bigint * 8192 FROM pg_settings WHERE name = 'shared_buffers';")
	harness.Detail(t, "shared_buffers_bytes", sharedBuffers)

	harness.Step(t, "pgbench -i -s %d", gracefulRebootScale)
	harness.PGBench(t, client, conn, "-i", "-q", "-s", strconv.Itoa(gracefulRebootScale))

	// Assert the load is testing something. A scale factor that quietly built a
	// database smaller than the buffer pool leaves nothing to write out on the
	// way down, and the test's name stops matching what it does.
	dbBytes := queryBytes(t, client, conn, "SELECT pg_database_size(current_database());")
	harness.Detail(t, "database_bytes", dbBytes)
	require.Greaterf(t, dbBytes, sharedBuffers,
		"the load built %d bytes against %d bytes of shared buffers, so the pool was never under pressure "+
			"and this run does not exercise a loaded shutdown", dbBytes, sharedBuffers)

	harness.Step(t, "pgbench -T %s to dirty the buffer pool", gracefulRebootDirtyRun)
	harness.PGBench(t, client, conn, "-T", gracefulRebootDirtyRun, "-c", "4", "-j", "2")

	harness.Phase(t, "Writing the unlogged probe")
	harness.PSQL(t, client, conn, fmt.Sprintf(
		"CREATE UNLOGGED TABLE %s (id int primary key, at timestamptz default now()); "+
			"INSERT INTO %s (id) SELECT generate_series(1, %d);",
		gracefulRebootTable, gracefulRebootTable, gracefulRebootRows))

	require.Equalf(t, gracefulRebootRows, countRows(t, client, conn, gracefulRebootTable),
		"the probe table did not hold its rows before the reboot, so nothing after this proves anything")

	before := postmasterStartTime(t, client, conn)

	harness.Phase(t, "Rebooting %q while its buffer pool is dirty", id)
	_, err := f.AWS.RDS.RebootDBInstance(&rds.RebootDBInstanceInput{DBInstanceIdentifier: aws.String(id)})
	require.NoError(t, err, "reboot-db-instance")
	waitForAvailable(t, f, id)

	// Without this the row count proves nothing: an engine that never restarted
	// would of course still hold them.
	after := postmasterStartTime(t, client, conn)
	require.NotEqual(t, before, after,
		"pg_postmaster_start_time() did not move, so the engine never restarted and the probe is meaningless")

	assert.Equalf(t, gracefulRebootRows, countRows(t, client, conn, gracefulRebootTable),
		"the unlogged probe table lost its rows across the reboot, which is what PostgreSQL does to one "+
			"on crash recovery — the engine was reset rather than asked to shut down")
}

// queryBytes runs a single-value query and returns it as an integer.
func queryBytes(t *testing.T, tgt harness.SSHTarget, conn harness.PSQLConn, sql string) int64 {
	t.Helper()
	out := strings.TrimSpace(harness.PSQL(t, tgt, conn, sql))
	n, err := strconv.ParseInt(out, 10, 64)
	require.NoErrorf(t, err, "parse %q as an integer", out)
	return n
}

// countRows returns how many rows table holds. An unlogged table truncated by
// crash recovery still exists, so this reads zero rather than failing.
func countRows(t *testing.T, tgt harness.SSHTarget, conn harness.PSQLConn, table string) int {
	t.Helper()
	return int(queryBytes(t, tgt, conn, "SELECT count(*) FROM "+table+";"))
}
