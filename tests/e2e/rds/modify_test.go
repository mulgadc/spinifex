//go:build e2e

package rds

import (
	"fmt"
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
	// The table both modifications are asserted across: a grow that loses it has
	// grown the wrong volume, and a class change that loses it has replaced the VM
	// without re-attaching the data (D9).
	modifyTable = "e2e_modify"
	modifyNote  = "written before the grow"

	// The grow target, from the suite's 20 GiB default. Ten GiB is enough to be
	// unambiguous in the volume's reported size and small enough that the resize
	// costs nothing.
	grownStorageGiB = 30

	// The one place in the suite a second class is meaningful: it is the class D20
	// exists to protect, and one step up from the D15 floor.
	grownClass = "db.t3.small"

	// shared_buffers is a quarter of class memory, so it is the parameter that
	// proves the size-derived defaults were re-resolved against the new class
	// rather than carried over: db.t3.micro has 1 GiB and db.t3.small has 2.
	sharedBuffersAtFloor = "256MB"
	sharedBuffersAtSmall = "512MB"
)

// TestModifyStorageAndClass drives the two disruptive modifications, both with
// ApplyImmediately: a storage grow, which restarts the VM it already has, and a
// class change, which replaces it. What only a live cluster can prove is that the
// volume behind the instance really grew, that the endpoint survives a VM being
// swapped out from under it, and that the engine comes back on defaults derived
// from the class it is now on.
//
// The instance is consumed: it ends up on a different class and a larger volume
// than it was created with.
//
// The filesystem-level half of the grow is deliberately not here. Proving in SQL
// that the guest's filesystem grew means writing enough data to exceed the old
// volume, which does not belong in a PR gate; the runbook reads df -h off the
// serial console instead.
func TestModifyStorageAndClass(t *testing.T) {
	f := requireRDSFixture(t)
	id := fmt.Sprintf("%s-modify-%d", dbInstancePfx, time.Now().Unix())

	harness.Phase(t, "Creating DB instance %q at %d GiB on %s", id, dbStorageGiB, dbClass)
	createDBInstance(t, f, id)
	client := harness.RDSClientVM(t, f.AWS, f.Harness, f.Env)
	system := f.SystemAWS(t)

	instance := harness.WaitForDBInstanceAvailable(t, f.AWS, id)
	conn := harness.PSQLConnFor(t, instance, dbMasterUser, dbMasterPassword, dbName)
	endpoint := conn.Host
	require.Equal(t, int64(dbStorageGiB), aws.Int64Value(instance.AllocatedStorage))
	require.Equal(t, dbClass, aws.StringValue(instance.DBInstanceClass))

	// The baseline every assertion below is a difference from. The VM carries no
	// per-instance tag, so its ID can only be held from while it was alive — which
	// is what makes "the same VM" and "a new VM" distinguishable at all.
	vmID := aws.StringValue(harness.DBInstanceVM(t, system, id).InstanceId)
	dataVolume := harness.DBInstanceDataVolume(t, system, id)
	dataVolumeID := aws.StringValue(dataVolume.VolumeId)
	require.Equal(t, int64(dbStorageGiB), aws.Int64Value(dataVolume.Size),
		"the data volume must start at the size the create asked for")
	privateIP := aws.StringValue(harness.DBEndpointENI(t, f.AWS, id).PrivateIpAddress)

	harness.PSQL(t, client, conn, fmt.Sprintf(
		"CREATE TABLE %s (id int primary key, note text); INSERT INTO %s VALUES (1, '%s');",
		modifyTable, modifyTable, modifyNote))

	// D12: there is no online grow, so a grow is a stop, a ModifyVolume the volume
	// will only accept while nothing holds it, and a start. The VM is restarted
	// rather than replaced — only a class change replaces it.
	t.Run("AGrowEnlargesTheVolumeUnderTheSameVM", func(t *testing.T) {
		harness.Phase(t, "Growing %q from %d to %d GiB", id, dbStorageGiB, grownStorageGiB)
		out, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: aws.String(id),
			AllocatedStorage:     aws.Int64(grownStorageGiB),
			ApplyImmediately:     aws.Bool(true),
		})
		require.NoError(t, err, "modify-db-instance storage")
		require.NotNil(t, out.DBInstance)

		// The call returns once the volume is at its new size and the VM is coming
		// back up, so the storage is already the new one and the instance is not yet
		// available. A customer polling this is watching an outage they were told to
		// expect, which is why the status has to say so.
		assert.Equal(t, harness.DBInstanceModifying, aws.StringValue(out.DBInstance.DBInstanceStatus),
			"a grow takes the engine down, so the modify must report the instance as modifying")
		assert.Equal(t, int64(grownStorageGiB), aws.Int64Value(out.DBInstance.AllocatedStorage))
		// The in-guest filesystem grow is still outstanding here, and is deliberately
		// not reported as pending: the customer's storage is the new size, so
		// reporting it as pending would show Terraform drift on a change that landed.
		assert.Nil(t, out.DBInstance.PendingModifiedValues,
			"a grow that has reached the volume must not still read as pending")

		grown := harness.WaitForDBInstanceAvailable(t, f.AWS, id)
		assert.Equal(t, int64(grownStorageGiB), aws.Int64Value(grown.AllocatedStorage),
			"the describe must report the grown size once the instance is back")
		assert.Nil(t, grown.PendingModifiedValues, "nothing is outstanding once the filesystem grow has run")

		volume := harness.DBInstanceDataVolume(t, system, id)
		assert.Equal(t, dataVolumeID, aws.StringValue(volume.VolumeId),
			"a grow must enlarge the volume the instance already has, not replace it")
		assert.Equal(t, int64(grownStorageGiB), aws.Int64Value(volume.Size),
			"the volume behind the instance is the only proof the grow was more than a record write")

		assert.Equal(t, vmID, aws.StringValue(harness.DBInstanceVM(t, system, id).InstanceId),
			"a grow restarts the VM it has; only a class change replaces it")

		note := harness.PSQL(t, client, conn, fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", modifyTable))
		assert.Equal(t, modifyNote, strings.TrimSpace(note), "the grow must not lose the datadir")

		// The agent ran the last step of the grow, and this event is the only report
		// a customer gets that it did. It is recorded before the instance goes back
		// to available, so it is already there.
		assertGrowEventRecorded(t, f, id)
	})

	// D15 and D20 in one assertion: the class moves, which replaces the VM, and the
	// size-derived defaults have to be re-resolved against the class the instance is
	// becoming. D12 keeps the endpoint through it — the data volume and the customer
	// ENI are re-attached to the replacement, so the address clients hold is unmoved.
	t.Run("AClassChangeReplacesTheVMAndReResolvesTheDefaults", func(t *testing.T) {
		assert.Equal(t, sharedBuffersAtFloor, showParameter(t, client, conn, "shared_buffers"),
			"the engine must start out on the defaults derived from %s", dbClass)

		harness.Phase(t, "Changing the class of %q from %s to %s", id, dbClass, grownClass)
		out, err := f.AWS.RDS.ModifyDBInstance(&rds.ModifyDBInstanceInput{
			DBInstanceIdentifier: aws.String(id),
			DBInstanceClass:      aws.String(grownClass),
			ApplyImmediately:     aws.Bool(true),
		})
		require.NoError(t, err, "modify-db-instance class")
		require.NotNil(t, out.DBInstance)
		assert.Equal(t, harness.DBInstanceModifying, aws.StringValue(out.DBInstance.DBInstanceStatus),
			"the replacement VM has to boot and report healthy before the instance is available again")
		assert.Equal(t, grownClass, aws.StringValue(out.DBInstance.DBInstanceClass))

		changed := harness.WaitForDBInstanceAvailable(t, f.AWS, id)
		assert.Equal(t, grownClass, aws.StringValue(changed.DBInstanceClass))
		assert.Equal(t, int64(grownStorageGiB), aws.Int64Value(changed.AllocatedStorage),
			"a class change must not disturb the storage a previous grow landed")

		require.NotNil(t, changed.Endpoint)
		assert.Equal(t, endpoint, aws.StringValue(changed.Endpoint.Address),
			"the endpoint is the customer's handle on the instance and must survive the VM being replaced")
		assert.Equal(t, privateIP, aws.StringValue(harness.DBEndpointENI(t, f.AWS, id).PrivateIpAddress),
			"D5: the ENI is re-attached to the replacement, so the address the name resolves to is unchanged")
		assert.Equal(t, dataVolumeID, aws.StringValue(harness.DBInstanceDataVolume(t, system, id).VolumeId),
			"D9: the data volume is re-attached rather than rebuilt")

		replacementID := aws.StringValue(harness.DBInstanceVM(t, system, id).InstanceId)
		assert.NotEqual(t, vmID, replacementID, "a class change is delivered by replacing the VM")
		// The old VM has to actually go: one left running would still hold the class
		// the customer has stopped paying for, and D3's index rewrite assumes it is
		// gone rather than merely superseded.
		harness.AssertVMGone(t, system, vmID)

		note := harness.PSQL(t, client, conn, fmt.Sprintf("SELECT note FROM %s WHERE id = 1;", modifyTable))
		assert.Equal(t, modifyNote, strings.TrimSpace(note),
			"the row written before both modifications must survive the replace")

		// The whole point of D20: the defaults are formulas over class memory, so a
		// class change that did not re-resolve them leaves a 2 GiB instance running a
		// 1 GiB configuration.
		assert.Equal(t, sharedBuffersAtSmall, showParameter(t, client, conn, "shared_buffers"),
			"the engine must be running the defaults derived from %s", grownClass)

		// The re-resolved set includes static settings, and the replacement booted on
		// it — so there is nothing left for a reboot to apply. Reporting otherwise
		// leaves a customer looking at a pending change that has already landed, and
		// rebooting a healthy database to clear it.
		require.NotEmpty(t, changed.DBParameterGroups)
		assert.Equal(t, "in-sync", aws.StringValue(changed.DBParameterGroups[0].ParameterApplyStatus),
			"the VM replace is the restart that applied the new class's static parameters")
	})
}

// The event the reconciler records once the agent has extended the guest's
// filesystem onto the grown volume. Nothing else in the suite can see that step
// happen, so its absence is the assertion that catches a grow which stopped at
// the volume.
func assertGrowEventRecorded(t *testing.T, f *Fixture, id string) {
	t.Helper()
	out, err := f.AWS.RDS.DescribeEvents(&rds.DescribeEventsInput{
		SourceType:       aws.String("db-instance"),
		SourceIdentifier: aws.String(id),
		Duration:         aws.Int64(1440),
	})
	require.NoError(t, err, "describe-events")
	require.NotEmpty(t, out.Events)

	for _, event := range out.Events {
		if !strings.Contains(aws.StringValue(event.Message), "storage grown") {
			continue
		}
		assert.Contains(t, aws.StringValueSlice(event.EventCategories), "configuration change",
			"a grow is a configuration change, which is the category a customer subscribes on")
		return
	}
	t.Errorf("no storage-grown event was recorded against %s; the filesystem grow never reported", id)
}

// A parameter as the running engine reports it. SHOW renders a memory setting in
// whichever unit fits, which is also how a customer reads it back.
func showParameter(t *testing.T, tgt harness.SSHTarget, conn harness.PSQLConn, parameter string) string {
	t.Helper()
	return strings.TrimSpace(harness.PSQL(t, tgt, conn, "SHOW "+parameter+";"))
}
