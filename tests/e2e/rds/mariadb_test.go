//go:build e2e

package rds

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/tests/e2e/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// The family a MariaDB parameter group belongs to, which is the engine and
	// its pinned series exactly as AWS RDS names it.
	mariaFamily         = mariaEngine + mariaVersion
	mariaDefaultGroup   = "default." + mariaFamily
	mariaInstancePfx    = dbInstancePfx + "-maria"
	mariaDBName         = "orders"
	mariaMasterUser     = "appuser"
	mariaMasterPassword = "e2eM4ria'DB\\Secret1"

	// Both passwords carry the two characters ValidateMasterUserPassword permits
	// and no MariaDB client can quote on our behalf, so rds-init's escaping and
	// the agent's rotation are under test rather than assumed.
	mariaRotatedPassword = "e2eR0tated\\Maria'2"

	// The table every MariaDB assertion is made against: a stop, a start or a VM
	// replacement that loses it has not preserved the datadir.
	mariaTable = "e2e_mariadb"
	mariaNote  = "hello from the client VM"

	// A dynamic parameter, adopted with SET GLOBAL, so the assertion does not
	// have to wait out a restart. Neither this nor the static one below is a
	// setting mysqld rewrites at startup, which would otherwise read as a value
	// the customer set never reaching the engine.
	mariaDynamicParameter = "wait_timeout"
	mariaDynamicValue     = "3600"

	// A static one — the engine adopts it only on a restart, which is what makes
	// it the parameter that can be pending-reboot.
	mariaStaticParameter = "innodb_read_io_threads"
	mariaStaticValue     = "8"

	// MariaDB's enforcement parameter, under AWS's own name, and the phrase the
	// server's refusal of an unencrypted connection carries.
	mariaTLSParameter = "require_secure_transport"
	mariaTLSRefusal   = "secure transport"
)

// Versions that exist upstream but are not what the image runs: two other
// series, and a minor of the pinned one. Each has to be refused rather than
// served by an image running something else.
var mariaUnpinnedVersions = []string{"11.4", "10.11", "11.8.8"}

// The MariaDB create request, valid as it stands so a caller mutates only the
// field it cares about. Same class and storage as the PostgreSQL spec: the
// engine is what is under test, not the sizing.
func validMariaDBCreateInput(id string) *rds.CreateDBInstanceInput {
	return &rds.CreateDBInstanceInput{
		DBInstanceIdentifier: aws.String(id),
		Engine:               aws.String(mariaEngine),
		DBInstanceClass:      aws.String(dbClass),
		AllocatedStorage:     aws.Int64(dbStorageGiB),
		DBName:               aws.String(mariaDBName),
		MasterUsername:       aws.String(mariaMasterUser),
		MasterUserPassword:   aws.String(mariaMasterPassword),
	}
}

// TestMariaDBInstance is the second engine's own leg of the suite: one instance
// carried from create through a client connection, a password rotation, an
// attached parameter group, a stop/start and a VM replacement.
//
// It is one test rather than several because every assertion in it is about the
// same database, and a DB VM is the suite's scarcest resource. The instance is
// consumed: it ends on a larger class with a rotated password and both
// overrides in force.
func TestMariaDBInstance(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	// One instance throughout — the class change replaces the VM behind it rather
	// than adding a second — reserved at the class it ends on, since that is the
	// peak the replacement has to find room for.
	reserveDBVMs(t, grownClass)

	suffix := time.Now().Unix()
	id := fmt.Sprintf("%s-%d", mariaInstancePfx, suffix)
	paramGroup := fmt.Sprintf("%s-params-%d", mariaInstancePfx, suffix)

	// Created before the instance so its teardown is registered first and LIFO
	// runs it last: a group delete issued while the instance still holds it is
	// refused.
	createMariaDBParameterGroup(t, f, paramGroup)

	harness.Phase(t, "Creating MariaDB instance %q", id)
	created := createDBInstanceFrom(t, f, validMariaDBCreateInput(id), func(in *rds.CreateDBInstanceInput) {
		in.DBParameterGroupName = aws.String(paramGroup)
	})
	assert.Equal(t, harness.DBInstanceCreating, aws.StringValue(created.DBInstanceStatus))

	client := rdsClient(t, f)
	system := f.SystemAWS(t)
	instance := waitForAvailable(t, f, id)

	// Reassigned by the rotation subtest, so every subtest after it connects with
	// the credential the platform last installed rather than the one that created
	// the instance.
	conn := harness.MariaDBConnFor(t, instance, mariaMasterUser, mariaMasterPassword, mariaDBName)
	endpoint := conn.Host

	// Captured while the instance is up: the VM carries no per-instance tag, so
	// after a replacement it can only be recognised by an ID held from before.
	vmID := aws.StringValue(harness.DBInstanceVM(t, system, id).InstanceId)
	dataVolumeID := aws.StringValue(harness.DBInstanceDataVolume(t, system, id).VolumeId)
	privateIP := aws.StringValue(harness.DBEndpointENI(t, f.AWS, id).PrivateIpAddress)

	t.Run("DescribesAsMariaDBOnItsOwnPort", func(t *testing.T) {
		assert.Equal(t, mariaEngine, aws.StringValue(instance.Engine),
			"the engine identifier is mariadb; mysql is not offered as an alias for it")
		assert.Equal(t, mariaVersion, aws.StringValue(instance.EngineVersion))
		assert.Equal(t, dbClass, aws.StringValue(instance.DBInstanceClass))
		assert.Equal(t, int64(dbStorageGiB), aws.Int64Value(instance.AllocatedStorage))
		assert.Equal(t, mariaMasterUser, aws.StringValue(instance.MasterUsername))
		assert.True(t, aws.BoolValue(instance.StorageEncrypted))
		assert.False(t, aws.BoolValue(instance.MultiAZ))
		assert.False(t, aws.BoolValue(instance.PubliclyAccessible))
		assert.Equal(t, paramGroup, dbParameterGroupName(instance))

		require.NotNil(t, instance.Endpoint, "an available instance must publish an endpoint")
		assert.Equal(t, int64(harness.MariaDBEnginePort), aws.Int64Value(instance.Endpoint.Port),
			"MariaDB's default port is its own, not the other engine's")
	})

	// The first assertion that proves an engine is actually serving: every status
	// before this one is the control plane's own account of itself.
	t.Run("AClientInTheVPCRoundTripsARow", func(t *testing.T) {
		harness.MariaDB(t, client, conn, fmt.Sprintf(
			"CREATE TABLE %s (id int primary key, note text); INSERT INTO %s VALUES (1, '%s');",
			mariaTable, mariaTable, mariaNote))

		assert.Equal(t, mariaNote, mariaValue(t, client, conn,
			fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", mariaTable)),
			"the row written over the endpoint must read back")

		// default_storage_engine is platform-owned and pinned to InnoDB because
		// the snapshot guarantee rests on it: a table created without an explicit
		// ENGINE= clause has to land somewhere with a redo log.
		assert.Equal(t, "InnoDB", mariaValue(t, client, conn, fmt.Sprintf(
			"SELECT engine FROM information_schema.tables WHERE table_schema = '%s' AND table_name = '%s';",
			mariaDBName, mariaTable)),
			"a table created with no ENGINE clause must be InnoDB")
	})

	// Stock MariaDB cannot exclude mysql.* from a global object grant. The
	// protected routine is the only path to privileges on another database.
	t.Run("MasterPrivilegesCannotReachSystemAccounts", func(t *testing.T) {
		for name, sql := range map[string]string{
			"ReadGrantTable":  "SELECT Priv FROM mysql.global_priv LIMIT 1;",
			"WriteGrantTable": "UPDATE mysql.global_priv SET Priv = Priv WHERE User = 'root';",
			"AlterRoot":       "ALTER USER 'root'@'localhost' IDENTIFIED BY 'not-allowed';",
			"DropRoutine":     "DROP PROCEDURE `_spinifex_rds`.`create_database`;",
			"PlainCreate":     "CREATE DATABASE forbidden_plain_database;",
		} {
			t.Run(name, func(t *testing.T) {
				out, err := harness.TryMariaDB(client, conn, sql)
				require.Error(t, err, "master statement unexpectedly succeeded: %s", out)
				assert.Contains(t, strings.ToLower(out), "denied")
			})
		}

		harness.MariaDB(t, client, conn, "FLUSH PRIVILEGES;")
		futureDB := "e2e_future_database"
		harness.MariaDB(t, client, conn,
			fmt.Sprintf("CALL `_spinifex_rds`.`create_database`('%s');", futureDB))
		future := conn
		future.DBName = futureDB
		harness.MariaDB(t, client, future,
			"CREATE TABLE future_table (id int primary key); INSERT INTO future_table VALUES (1);")
		assert.Equal(t, "1", mariaValue(t, client, future, "SELECT id FROM future_table;"))

		out, err := harness.TryMariaDB(client, conn,
			"CALL `_spinifex_rds`.`create_database`('mysql');")
		require.Error(t, err, "system database creation unexpectedly succeeded: %s", out)
		assert.Contains(t, out, "non-system identifier")
	})

	// TLS is required rather than merely offered, so what is asserted is both
	// halves: a client asking for it gets it, and one refusing it is turned away.
	t.Run("AClientCanConnectOverTLSAndPlaintextIsRefused", func(t *testing.T) {
		encrypted := conn
		encrypted.SSLRootCert = harness.RDSClientCACertPath
		assert.NotEmpty(t, mariaSessionCipher(t, client, encrypted),
			"a connection made against the cluster CA must report a negotiated cipher")

		// The only direct proof enforcement is live. require_secure_transport is a
		// real global variable, so the agent can read it back — but only a
		// connection the server turns away proves it acted on it.
		assertMariaDBRefusesPlaintext(t, client, conn)

		// Verified by name only, in its own subtest because a cluster with no base
		// domain has no name to verify against. The endpoint name and the ENI
		// address are both in the certificate's SAN set, but the client's hostname
		// check is written against the certificate's DNS names, so pinning the
		// address here would assert something about the client's IP-SAN handling
		// rather than about ours.
		t.Run("AndTheCertificateVerifiesByName", func(t *testing.T) {
			requireEndpointName(t, endpoint)
			verified := encrypted
			verified.VerifyServerCert = true
			assert.NotEmpty(t, mariaSessionCipher(t, client, verified),
				"the serving certificate must verify against the cluster CA under the endpoint name")
		})
	})

	// Enforcement is the customer's to turn off, as it is on AWS, and SET GLOBAL
	// lands both directions on the running server without a restart.
	t.Run("EnforcementFlipsWithoutARestart", func(t *testing.T) {
		plaintext := conn
		plaintext.Plaintext = true

		// Registered before the flip: a failure part-way would otherwise leave every
		// later subtest on a database serving in the clear.
		t.Cleanup(func() { setGroupParameter(t, f, paramGroup, mariaTLSParameter, "1", "immediate") })

		harness.Step(t, "Turning %s off on %q", mariaTLSParameter, id)
		setGroupParameter(t, f, paramGroup, mariaTLSParameter, "0", "immediate")
		// The cipher, not merely a successful connection: it is what separates a
		// client that really connected in the clear from one that quietly negotiated
		// TLS anyway, which would make the assertion above prove nothing.
		assert.Empty(t, mariaSessionCipher(t, client, plaintext),
			"with enforcement off the same connection must be accepted, and in the clear")

		harness.Step(t, "Turning %s back on on %q", mariaTLSParameter, id)
		setGroupParameter(t, f, paramGroup, mariaTLSParameter, "1", "immediate")
		assertMariaDBRefusesPlaintext(t, client, conn)
	})

	// The system keeps no cleartext password anywhere, so the only proof a
	// rotation reached the engine is that the new credential authenticates and
	// the old one does not.
	t.Run("TheMasterPasswordRotates", func(t *testing.T) {
		_, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: aws.String(id),
			MasterUserPassword:   aws.String(mariaRotatedPassword),
			ApplyImmediately:     aws.Bool(true),
		})
		require.NoError(t, err, "modify-db-instance master password")

		rotated := conn
		rotated.Password = mariaRotatedPassword
		assert.Equal(t, mariaNote, mariaValue(t, client, rotated,
			fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", mariaTable)),
			"the rotated password must reach the same database")

		out, err := harness.TryMariaDB(client, conn, "SELECT 1;")
		require.Error(t, err, "the retired password must not connect: %s", out)
		assert.Contains(t, out, "Access denied",
			"the old password must be refused by the engine, not by the network")

		conn = rotated
	})

	// The family check is one funnel every binding path goes through, and modify
	// is the one a customer reaches by hand. Non-disruptive: it is refused before
	// anything is written, so the instance is left where it was found.
	t.Run("APostgreSQLParameterGroupCannotBeAttached", func(t *testing.T) {
		harness.ExpectError(t, "InvalidParameterCombination", func() error {
			_, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
				DBInstanceIdentifier: aws.String(id),
				DBParameterGroupName: aws.String("default." + dbParameterGroupFamily),
				ApplyImmediately:     aws.Bool(true),
			})
			return err
		})

		current, err := harness.DescribeDBInstance(f.AWS, id)
		require.NoError(t, err)
		assert.Equal(t, paramGroup, dbParameterGroupName(current),
			"a refused attachment must leave the instance on the group it already had")
	})

	// The only assertions that prove a resolved parameter reached the engine
	// rather than merely being stored, one per apply method.
	t.Run("AttachedGroupEditsReachTheRunningEngine", func(t *testing.T) {
		require.NotEqual(t, mariaDynamicValue, mariaGlobal(t, client, conn, mariaDynamicParameter),
			"the chosen dynamic value must differ from the catalog default")

		setGroupParameter(t, f, paramGroup, mariaDynamicParameter, mariaDynamicValue, "immediate")
		assert.Equal(t, mariaDynamicValue, mariaGlobal(t, client, conn, mariaDynamicParameter),
			"an immediate edit to an attached group must reload without a reboot")

		before := mariaGlobal(t, client, conn, mariaStaticParameter)
		require.NotEqual(t, mariaStaticValue, before,
			"the chosen static value must differ from the catalog default")

		setGroupParameter(t, f, paramGroup, mariaStaticParameter, mariaStaticValue, "pending-reboot")
		pending, err := harness.DescribeDBInstance(f.AWS, id)
		require.NoError(t, err, "describe the pending static parameter")
		require.NotEmpty(t, pending.DBParameterGroups)
		assert.Equal(t, "pending-reboot", aws.StringValue(pending.DBParameterGroups[0].ParameterApplyStatus))
		assert.Equal(t, before, mariaGlobal(t, client, conn, mariaStaticParameter),
			"a static edit must not take effect before a reboot")

		_, err = f.AWS.RDS.RebootDBInstance(&rds.RebootDBInstanceInput{DBInstanceIdentifier: aws.String(id)})
		require.NoError(t, err, "reboot-db-instance for the static parameter")
		instance = waitForAvailable(t, f, id)

		require.NotEmpty(t, instance.DBParameterGroups)
		assert.Equal(t, "in-sync", aws.StringValue(instance.DBParameterGroups[0].ParameterApplyStatus),
			"the reboot applied the pending parameter, so nothing is outstanding")
		assertMariaDBOverridesInForce(t, client, conn)
	})

	// A stop keeps the data volume, the customer ENI and its address, so a start
	// comes back on the same datadir at the same endpoint. The VM is stopped
	// rather than terminated — only a class change replaces it.
	t.Run("StopAndStartKeepTheDataTheAddressAndTheOverrides", func(t *testing.T) {
		_, err := f.AWS.RDS.StopDBInstance(&rds.StopDBInstanceInput{DBInstanceIdentifier: aws.String(id)})
		require.NoError(t, err, "stop-db-instance")
		stopped := harness.WaitForDBInstanceStatus(t, f.AWS, id, harness.DBInstanceStopped)

		harness.WaitForInstanceState(t, system, vmID, ec2.InstanceStateNameStopped)
		assert.Equal(t, dataVolumeID, aws.StringValue(harness.DBInstanceDataVolume(t, system, id).VolumeId),
			"the data volume must be retained across a stop")
		require.NotNil(t, stopped.Endpoint, "a stopped instance still reports where it will be when it comes back")
		assert.Equal(t, endpoint, aws.StringValue(stopped.Endpoint.Address))

		_, err = f.AWS.RDS.StartDBInstance(&rds.StartDBInstanceInput{DBInstanceIdentifier: aws.String(id)})
		require.NoError(t, err, "start-db-instance")
		started := waitForAvailable(t, f, id)

		require.NotNil(t, started.Endpoint)
		assert.Equal(t, endpoint, aws.StringValue(started.Endpoint.Address), "the endpoint must survive a stop/start")
		assert.Equal(t, privateIP, aws.StringValue(harness.DBEndpointENI(t, f.AWS, id).PrivateIpAddress),
			"the private address is what the endpoint name resolves to, so it must persist")

		assert.Equal(t, mariaNote, mariaValue(t, client, conn,
			fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", mariaTable)),
			"the row written before the stop must still be there")
		assertMariaDBOverridesInForce(t, client, conn)
		// Enforcement is derived at boot from the parameter file on the data volume,
		// so a start that came back accepting plaintext would be a healthy instance
		// serving a connection its owner's configuration forbids.
		assertMariaDBRefusesPlaintext(t, client, conn)
	})

	// The class moves, which replaces the VM. The endpoint is kept through it —
	// the data volume and the customer ENI are re-attached to the replacement —
	// and so are the parameters the customer set, which is what putting the
	// generated configuration on the data volume exists to guarantee: nothing
	// issues an apply on a plain replace, so a configuration living on the boot
	// volume would be discarded with it and the engine would silently revert to
	// catalog defaults.
	t.Run("AClassChangeReplacesTheVMAndKeepsTheOverrides", func(t *testing.T) {
		poolAtFloor := mariaGlobalInt(t, client, conn, "innodb_buffer_pool_size")

		harness.Phase(t, "Changing the class of %q from %s to %s", id, dbClass, grownClass)
		out, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: aws.String(id),
			DBInstanceClass:      aws.String(grownClass),
			ApplyImmediately:     aws.Bool(true),
		})
		require.NoError(t, err, "modify-db-instance class")
		require.NotNil(t, out.DBInstance)
		assert.Equal(t, harness.DBInstanceModifying, aws.StringValue(out.DBInstance.DBInstanceStatus))

		changed := waitForAvailable(t, f, id)
		assert.Equal(t, grownClass, aws.StringValue(changed.DBInstanceClass))
		require.NotNil(t, changed.Endpoint)
		assert.Equal(t, endpoint, aws.StringValue(changed.Endpoint.Address),
			"the endpoint is the customer's handle on the instance and must survive the VM being replaced")
		assert.Equal(t, privateIP, aws.StringValue(harness.DBEndpointENI(t, f.AWS, id).PrivateIpAddress),
			"the ENI is re-attached to the replacement, so the address the name resolves to is unchanged")
		assert.Equal(t, dataVolumeID, aws.StringValue(harness.DBInstanceDataVolume(t, system, id).VolumeId),
			"the data volume is re-attached rather than rebuilt")

		replacementID := aws.StringValue(harness.DBInstanceVM(t, system, id).InstanceId)
		assert.NotEqual(t, vmID, replacementID, "a class change is delivered by replacing the VM")
		harness.AssertVMGone(t, system, vmID)

		assert.Equal(t, mariaNote, mariaValue(t, client, conn,
			fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", mariaTable)),
			"the row written before the replacement must survive it")
		assertMariaDBOverridesInForce(t, client, conn)
		assertMariaDBRefusesPlaintext(t, client, conn)

		// The size-derived defaults are re-resolved against the class the instance
		// is now on, so a replacement that carried the old configuration over
		// wholesale would leave a 2 GiB instance running a 1 GiB buffer pool. The
		// assertion is the direction rather than a literal: the server rounds the
		// pool to its own granularity, which is not ours to predict.
		assert.Greater(t, mariaGlobalInt(t, client, conn, "innodb_buffer_pool_size"), poolAtFloor,
			"the buffer pool default must be re-resolved against %s", grownClass)

		require.NotEmpty(t, changed.DBParameterGroups)
		assert.Equal(t, "in-sync", aws.StringValue(changed.DBParameterGroups[0].ParameterApplyStatus),
			"the replacement booted on the re-resolved set, so there is nothing left for a reboot to apply")
	})
}

// TestMariaDBSnapshotRestore proves the snapshot path is engine-neutral where it
// claims to be and engine-aware where it must be: the snapshot carries MariaDB's
// own engine, version and port, and the instance restored from it comes up on
// that datadir holding the rows as of the moment it was taken and the
// credentials the source already had.
func TestMariaDBSnapshotRestore(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	// The source and the instance restored from its snapshot, which have to be
	// alive together for the restore to be provably a point in time.
	reserveDBVMs(t, dbClass, dbClass)

	suffix := time.Now().Unix()
	sourceID := fmt.Sprintf("%s-snapsrc-%d", mariaInstancePfx, suffix)
	restoredID := fmt.Sprintf("%s-restored-%d", mariaInstancePfx, suffix)
	snapshotID := fmt.Sprintf("%s-snapshot-%d", mariaInstancePfx, suffix)
	paramGroup := fmt.Sprintf("%s-restore-params-%d", mariaInstancePfx, suffix)

	harness.Phase(t, "Creating source MariaDB instance %q", sourceID)
	createDBInstanceFrom(t, f, validMariaDBCreateInput(sourceID))
	client := rdsClient(t, f)

	source := waitForAvailable(t, f, sourceID)
	sourceConn := harness.MariaDBConnFor(t, source, mariaMasterUser, mariaMasterPassword, mariaDBName)

	harness.MariaDB(t, client, sourceConn, fmt.Sprintf(
		"CREATE TABLE %s (id int primary key, note text); INSERT INTO %s VALUES (1, '%s');",
		snapshotTable, snapshotTable, rowBeforeSnapshot))

	harness.Phase(t, "Snapshotting %q as %q", sourceID, snapshotID)
	snapshot := createDBSnapshot(t, f, sourceID, snapshotID)

	// The snapshot has to carry enough of the instance to rebuild it, because the
	// instance may be gone by the time it is used — and a snapshot that recorded
	// the wrong engine would be restorable onto an image that cannot read it.
	assert.Equal(t, harness.DBSnapshotAvailable, aws.StringValue(snapshot.Status))
	assert.Equal(t, mariaEngine, aws.StringValue(snapshot.Engine))
	assert.Equal(t, mariaVersion, aws.StringValue(snapshot.EngineVersion))
	assert.Equal(t, int64(harness.MariaDBEnginePort), aws.Int64Value(snapshot.Port))
	assert.Equal(t, mariaMasterUser, aws.StringValue(snapshot.MasterUsername))

	// Written after the snapshot and therefore the row that must be missing from
	// the restore. Its absence is what separates a point-in-time copy from a
	// second reference to the live volume.
	harness.MariaDB(t, client, sourceConn, fmt.Sprintf(
		"INSERT INTO %s VALUES (2, '%s');", snapshotTable, rowAfterSnapshot))

	// The engine was available, so the quiesce had every opportunity to run and
	// the crash-consistent fallback — whose MariaDB wording warns that
	// non-transactional tables may not recover — must not have been reached.
	t.Run("TheSnapshotWasTakenOverAQuiescedEngine", func(t *testing.T) {
		events := dbInstanceEventMessages(t, f, sourceID)
		assert.NotContains(t, strings.Join(events, "\n"), crashConsistentNotice,
			"BACKUP STAGE must have held the engine; the crash-consistent fallback is for when it cannot")
	})

	t.Run("ARestoreHoldsTheDataAsOfTheSnapshot", func(t *testing.T) {
		// The source was enforcing when the snapshot was taken, so the parameter
		// file that says so is on the restored volume. Restoring onto a group that
		// turns enforcement off is what proves the guest re-derives it rather than
		// inheriting whatever the volume arrived with.
		createMariaDBParameterGroup(t, f, paramGroup)
		setGroupParameter(t, f, paramGroup, mariaTLSParameter, "0", "immediate")

		harness.Phase(t, "Restoring %q from %q", restoredID, snapshotID)
		restored := restoreFromSnapshot(t, f, restoredID, snapshotID,
			func(in *rds.RestoreDBInstanceFromDBSnapshotInput) {
				in.DBParameterGroupName = aws.String(paramGroup)
			})
		assert.Equal(t, harness.DBInstanceCreating, aws.StringValue(restored.DBInstanceStatus))

		instance := waitForAvailable(t, f, restoredID)
		assert.Equal(t, mariaEngine, aws.StringValue(instance.Engine))
		assert.Equal(t, mariaMasterUser, aws.StringValue(instance.MasterUsername))
		require.NotNil(t, instance.Endpoint)
		assert.NotEqual(t, sourceConn.Host, aws.StringValue(instance.Endpoint.Address),
			"the restore is its own instance and must publish its own endpoint")

		// No password was supplied to the restore and none could be: the account
		// and its hash came out of the datadir, so the source's credentials are
		// the restore's.
		conn := harness.MariaDBConnFor(t, instance, mariaMasterUser, mariaMasterPassword, mariaDBName)
		assert.Equal(t, strconv.Itoa(snapshotRowsBefore),
			mariaValue(t, client, conn, fmt.Sprintf("SELECT count(*) FROM %s;", snapshotTable)),
			"the restore must hold the rows the snapshot captured and nothing written after it")
		assert.Equal(t, rowBeforeSnapshot, mariaValue(t, client, conn,
			fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", snapshotTable)))

		assert.Equal(t, "2", mariaValue(t, client, sourceConn,
			fmt.Sprintf("SELECT count(*) FROM %s;", snapshotTable)),
			"the source is untouched by the restore reading from its snapshot")

		// Two instances on the same datadir and opposite enforcement. The cipher
		// rather than the connection alone: an empty one is what proves the client
		// really did connect in the clear.
		assertMariaDBRefusesPlaintext(t, client, sourceConn)
		plaintext := conn
		plaintext.Plaintext = true
		assert.Empty(t, mariaSessionCipher(t, client, plaintext),
			"a restore into a group that turns enforcement off must not keep enforcing")
	})
}

// TestMariaDBValidation is the second engine's negative matrix: the versions and
// the cross-engine parameter groups that must be refused, and the implicit
// default group registering MariaDB brings with it. It boots nothing, so it
// fails in seconds rather than after a VM.
func TestMariaDBValidation(t *testing.T) {
	f := requireRDSFixture(t)
	t.Parallel()
	// Every create below is asserted to be refused, so in a passing run nothing
	// boots. The reservation covers the run where one is wrongly accepted:
	// expectCreateRefused deletes it, but the VM is charged to the suite until it
	// does.
	reserveDBVMs(t, dbClass)

	suffix := time.Now().Unix()
	rejectedID := fmt.Sprintf("%s-rejected-%d", mariaInstancePfx, suffix)
	mariaGroup := fmt.Sprintf("%s-family-%d", mariaInstancePfx, suffix)

	createMariaDBParameterGroup(t, f, mariaGroup)

	// Implicit rather than stored: registering the engine is what makes it
	// describable, and it must be so without ever having been created.
	t.Run("TheDefaultMariaDBGroupIsImplicitAndCarriesTheCatalog", func(t *testing.T) {
		out, err := f.AWS.RDS.DescribeDBParameterGroups(&rds.DescribeDBParameterGroupsInput{
			DBParameterGroupName: aws.String(mariaDefaultGroup),
		})
		require.NoError(t, err, "describe-db-parameter-groups")
		require.Len(t, out.DBParameterGroups, 1)
		assert.Equal(t, mariaFamily, aws.StringValue(out.DBParameterGroups[0].DBParameterGroupFamily))

		params := describeAllParameters(t, f, mariaDefaultGroup)
		pool, ok := params["innodb_buffer_pool_size"]
		require.True(t, ok, "the MariaDB catalog must be reachable through its own default group")
		assert.Equal(t, "engine-default", aws.StringValue(pool.Source))
		assert.Equal(t, "static", aws.StringValue(pool.ApplyType))
		assert.NotContains(t, aws.StringValue(pool.ParameterValue), "{",
			"a size-derived default must reach the customer as a literal")

		assert.NotContains(t, params, "shared_buffers",
			"the MariaDB catalog must not carry the other engine's parameters")
	})

	t.Run("AnUnpinnedEngineVersionIsRefused", func(t *testing.T) {
		for _, version := range mariaUnpinnedVersions {
			t.Run(version, func(t *testing.T) {
				in := validMariaDBCreateInput(rejectedID)
				in.EngineVersion = aws.String(version)
				expectCreateRefused(t, f, f.AWS, "InvalidParameterValue", in)
			})
		}
	})

	// Aliasing mysql onto the MariaDB image would report an engine and a version
	// the instance is not running, and the lie would propagate into the describe,
	// the parameter-group family and every snapshot taken of it.
	t.Run("MySQLIsNotAnAliasForMariaDB", func(t *testing.T) {
		in := validMariaDBCreateInput(rejectedID)
		in.Engine = aws.String(unsupportedDBEngine)
		expectCreateRefused(t, f, f.AWS, "InvalidParameterValue", in)
	})

	// Both directions, because the check is one funnel and a group carrying the
	// wrong engine's parameters would only fail once the engine refused to start.
	t.Run("AParameterGroupOfTheOtherEnginesFamilyIsRefusedAtCreate", func(t *testing.T) {
		mariaOnPostgres := validCreateInput(rejectedID)
		mariaOnPostgres.DBParameterGroupName = aws.String(mariaGroup)
		expectCreateRefused(t, f, f.AWS, "InvalidParameterCombination", mariaOnPostgres)

		postgresOnMaria := validMariaDBCreateInput(rejectedID)
		postgresOnMaria.DBParameterGroupName = aws.String("default." + dbParameterGroupFamily)
		expectCreateRefused(t, f, f.AWS, "InvalidParameterCombination", postgresOnMaria)
	})

	// The other engine's parameter names must not be storable into a MariaDB
	// group either: caught here, the customer learns of it on the group they got
	// wrong rather than on the instance that later refuses to start.
	t.Run("TheOtherEnginesParametersCannotBeStored", func(t *testing.T) {
		harness.ExpectError(t, "InvalidParameterValue", func() error {
			_, err := f.AWS.RDS.ModifyDBParameterGroup(&rds.ModifyDBParameterGroupInput{
				DBParameterGroupName: aws.String(mariaGroup),
				Parameters: []*rds.Parameter{{
					ParameterName:  aws.String("shared_buffers"),
					ParameterValue: aws.String("32768"),
				}},
			})
			return err
		})
	})
}

// An empty MariaDB parameter group and its teardown. Empty because what each
// test then stores into it is the assertion.
func createMariaDBParameterGroup(t *testing.T, f *Fixture, name string) {
	t.Helper()
	_, err := f.AWS.RDS.CreateDBParameterGroup(&rds.CreateDBParameterGroupInput{
		DBParameterGroupName:   aws.String(name),
		DBParameterGroupFamily: aws.String(mariaFamily),
		Description:            aws.String("rds e2e mariadb parameter group"),
	})
	require.NoError(t, err, "create-db-parameter-group %s", name)
	t.Cleanup(func() {
		if _, err := f.AWS.RDS.DeleteDBParameterGroup(&rds.DeleteDBParameterGroupInput{
			DBParameterGroupName: aws.String(name),
		}); err != nil && !harness.ErrorCodeIs(err, "DBParameterGroupNotFound") {
			t.Logf("cleanup: delete-db-parameter-group %s: %v", name, err)
		}
	})
}

// Both overrides as the running engine reports them. Asserted after every
// transition, because the failure this catches is silent: an instance that came
// back on catalog defaults is healthy, available and running a configuration
// its owner did not choose.
func assertMariaDBOverridesInForce(t *testing.T, tgt harness.SSHTarget, conn harness.MariaDBConn) {
	t.Helper()
	assert.Equal(t, mariaDynamicValue, mariaGlobal(t, tgt, conn, mariaDynamicParameter),
		"the dynamic override must still be in force")
	assert.Equal(t, mariaStaticValue, mariaGlobal(t, tgt, conn, mariaStaticParameter),
		"the static override must still be in force")
}

// The MariaDB half of the assertion every transition repeats. Matched on the
// reason the server gives rather than the whole message, which AWS words
// differently for the same rejection.
func assertMariaDBRefusesPlaintext(t *testing.T, tgt harness.SSHTarget, conn harness.MariaDBConn) {
	t.Helper()
	plaintext := conn
	plaintext.Plaintext = true
	out, err := harness.TryMariaDB(tgt, plaintext, "SELECT 1;")
	require.Error(t, err, "a plaintext connection must be refused: %s", out)
	assert.Contains(t, strings.ToLower(out), mariaTLSRefusal,
		"the server must turn the connection away for being unencrypted, not for some other reason")
}

// One global system variable as the running server reports it. GLOBAL rather
// than the session copy, since that is the scope a parameter group sets.
func mariaGlobal(t *testing.T, tgt harness.SSHTarget, conn harness.MariaDBConn, name string) string {
	t.Helper()
	return mariaValue(t, tgt, conn, "SELECT @@GLOBAL."+name+";")
}

func mariaGlobalInt(t *testing.T, tgt harness.SSHTarget, conn harness.MariaDBConn, name string) int64 {
	t.Helper()
	raw := mariaGlobal(t, tgt, conn, name)
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("@@GLOBAL.%s reported %q, which is not an integer", name, raw)
	}
	return value
}

// The cipher the connection negotiated, empty when it is in the clear. SHOW
// rather than performance_schema, which the catalog leaves off by default.
func mariaSessionCipher(t *testing.T, tgt harness.SSHTarget, conn harness.MariaDBConn) string {
	t.Helper()
	_, cipher, _ := strings.Cut(mariaValue(t, tgt, conn, "SHOW SESSION STATUS LIKE 'Ssl_cipher';"), "\t")
	return strings.TrimSpace(cipher)
}

// One scalar as the client printed it. The guest exec folds stderr into the same
// stream, so a client-side notice would otherwise be compared against the value:
// the answer is the last line the command produced.
func mariaValue(t *testing.T, tgt harness.SSHTarget, conn harness.MariaDBConn, sql string) string {
	t.Helper()
	out := harness.MariaDB(t, tgt, conn, sql)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
