package handlers_rds

import (
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testDataVolume   = "vol-rdsdata01"
	testEndpointENI  = "eni-cust01"
	testDefaultGroup = "default.postgres18"
)

// modifyHarness is a Service wired for day-two work: the launch fakes a replace
// needs, the customer VPC the security groups are validated against, the volume
// store a grow modifies, and a stub agent answering the command channel.
type modifyHarness struct {
	svc     *Service
	rec     *Reconciler
	nc      *nats.Conn
	launch  *launchHarness
	network *fakeNetwork
	iam     *fakeRDSEnsurer
	cmdr    *fakeInstanceCommander
	storage *fakeVolumeResizer
	vmState *fakeInstanceState
	agent   *stubAgent
}

func newModifyHarness(t *testing.T) *modifyHarness {
	return newModifyHarnessWithAgent(t, false)
}

// agentFails makes the stub agent reject every command, which is how a modify
// against a wedged guest is exercised without waiting out a real budget.
func newModifyHarnessWithAgent(t *testing.T, agentFails bool) *modifyHarness {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	vmState := &fakeInstanceState{state: instanceStateRunning}
	h := &modifyHarness{
		nc:      nc,
		launch:  newLaunchHarness(),
		network: newFakeNetwork(),
		iam:     &fakeRDSEnsurer{},
		cmdr:    &fakeInstanceCommander{vm: vmState},
		storage: newFakeVolumeResizer(testDataVolume, 20),
		vmState: vmState,
	}
	h.agent = newStubAgent(t, nc, testAccountID, testDBID, agentFails)
	h.svc = NewService(nc, testRegion).WithDeps(Deps{
		LoadCA:        newTestCA(t),
		Launch:        h.launch.deps(),
		Network:       h.network,
		IAM:           testIAMProvider(h.iam),
		Instances:     h.cmdr,
		Storage:       h.storage,
		InstanceState: h.vmState,
		VMStopTimeout: testVMStopTimeout,
	})
	h.rec = NewReconciler(h.svc, "node-a")
	return h
}

func (h *modifyHarness) record(t *testing.T) DBInstanceRecord {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(testDBID), &rec)
	require.NoError(t, err)
	require.True(t, found)
	return rec
}

func (h *modifyHarness) kv(t *testing.T) jetstream.KeyValue {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	return kv
}

// An available instance with everything a modify reads: the placement its
// security groups are checked against, the endpoint ENI they are re-associated
// on, and the data volume a grow resizes.
func modifiableRecord() DBInstanceRecord {
	rec := availableRecord()
	rec.DBInstanceClass = "db.t3.medium"
	rec.AllocatedStorage = 20
	rec.StorageType = storageTypeGP3
	rec.VpcID = testDefaultVPC
	rec.SubnetID = testDBSubnet
	rec.VpcSecurityGroupIDs = []string{testDefaultSG}
	rec.DBParameterGroupName = testDefaultGroup
	rec.ENIID = testEndpointENI
	rec.DataVolumeID = testDataVolume
	return rec
}

func modifyInput() *rds.ModifyDBInstanceInput {
	return &rds.ModifyDBInstanceInput{DBInstanceIdentifier: aws.String(testDBID)}
}

const largeMemoryParameterGroup = "large-memory"

func createLargeMemoryParameterGroup(t *testing.T, h *modifyHarness) {
	t.Helper()
	_, err := h.svc.CreateDBParameterGroup(t.Context(), parameterGroupInput(largeMemoryParameterGroup), testAccountID)
	require.NoError(t, err)
	_, err = h.svc.ModifyDBParameterGroup(t.Context(), modifyParameters(largeMemoryParameterGroup, &rds.Parameter{
		ParameterName:  aws.String("shared_buffers"),
		ParameterValue: aws.String("500000"),
		ApplyMethod:    aws.String(ApplyMethodPendingReboot),
	}), testAccountID)
	require.NoError(t, err)
}

// None of these interrupts service, so AWS applies them as soon as possible and
// so does this — ApplyImmediately is not what gates them.
func TestModifyDBInstance_AppliesTheNonDisruptiveSettingsAtOnce(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.MasterUserPassword = aws.String("N3w-Sup3rSecret!")
	in.DeletionProtection = aws.Bool(true)
	in.AutoMinorVersionUpgrade = aws.Bool(true)
	in.CopyTagsToSnapshot = aws.Bool(true)
	in.MonitoringInterval = aws.Int64(60)
	in.EnablePerformanceInsights = aws.Bool(true)
	in.BackupRetentionPeriod = aws.Int64(7)
	in.PreferredBackupWindow = aws.String("03:00-04:00")
	in.PreferredMaintenanceWindow = aws.String("sun:05:00-sun:06:00")

	out, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)
	require.NoError(t, err)

	// Still available: nothing here takes the engine down, so the instance never
	// leaves the state a client can connect to.
	assert.Equal(t, string(StatusAvailable), aws.StringValue(out.DBInstance.DBInstanceStatus))
	assert.Empty(t, h.cmdr.calls, "a non-disruptive modify does not touch the VM's power state")

	issued := h.agent.received()
	require.Len(t, issued, 1, "the password is applied live over the command channel")
	assert.Equal(t, CommandSetPassword, issued[0].Type)

	rec := h.record(t)
	assert.True(t, rec.DeletionProtection)
	assert.True(t, rec.AutoMinorVersionUpgrade)
	assert.True(t, rec.CopyTagsToSnapshot)
	assert.Equal(t, int64(60), rec.MonitoringInterval)
	assert.True(t, rec.EnablePerformanceInsights)
	assert.Equal(t, int64(7), rec.BackupRetentionPeriod)
	assert.Equal(t, "03:00-04:00", rec.PreferredBackupWindow)
	assert.Equal(t, "sun:05:00-sun:06:00", rec.PreferredMaintenanceWindow)
	assert.NotNil(t, rec.MasterPasswordUpdatedAt)
	assert.Nil(t, rec.PendingModifiedValues)
	assert.True(t, aws.BoolValue(out.DBInstance.CopyTagsToSnapshot))
	assert.Equal(t, int64(60), aws.Int64Value(out.DBInstance.MonitoringInterval))
	assert.True(t, aws.BoolValue(out.DBInstance.PerformanceInsightsEnabled))
}

// Only the fact of the rotation is recorded. A password that reached KV
// would be readable by anything that can read the bucket, forever.
func TestModifyDBInstance_NeverPersistsTheRotatedPassword(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifiableRecord()
	rec.Bootstrap.MasterUserPassword = ""
	seedInstance(t, h.svc, rec)

	in := modifyInput()
	in.MasterUserPassword = aws.String("N3w-Sup3rSecret!")
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)
	require.NoError(t, err)

	_, raw := readRecord(t, h.svc)
	assert.NotContains(t, raw, "N3w-Sup3rSecret!", "the rotated password must not be at rest anywhere")
}

// An agent that cannot be reached fails the call: a password change that is
// reported as applied but never reached the engine locks the customer out of
// their own database with no signal.
func TestModifyDBInstance_FailsWhenThePasswordCannotBeApplied(t *testing.T) {
	h := newModifyHarnessWithAgent(t, true)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.MasterUserPassword = aws.String("N3w-Sup3rSecret!")
	in.DeletionProtection = aws.Bool(true)
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)
	require.Error(t, err)

	// The rest of the request is not applied either: the record would otherwise
	// report a change the caller was told had failed.
	rec := h.record(t)
	assert.False(t, rec.DeletionProtection)
	assert.Nil(t, rec.MasterPasswordUpdatedAt)
}

// Changing a database's ingress is a live ENI re-association: no replace, no
// new address, so the endpoint clients resolve does not move.
func TestModifyDBInstance_ReassociatesTheSecurityGroupsOnTheEndpointENI(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.VpcSecurityGroupIds = aws.StringSlice([]string{"sg-app01"})
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)
	require.NoError(t, err)

	require.Len(t, h.launch.enis.modified, 1)
	assert.Equal(t, testEndpointENI, aws.StringValue(h.launch.enis.modified[0].NetworkInterfaceId))
	assert.Equal(t, []string{"sg-app01"}, aws.StringValueSlice(h.launch.enis.modified[0].Groups))
	assert.Empty(t, h.launch.enis.created, "the endpoint ENI is re-associated, not replaced")

	assert.Equal(t, []string{"sg-app01"}, h.record(t).VpcSecurityGroupIDs)
}

// An ENI cannot carry a group from another VPC, so the launch-time rule holds
// at modify too — and rejecting it here keeps the failure ahead of every write.
func TestModifyDBInstance_RejectsASecurityGroupFromAnotherVPC(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.VpcSecurityGroupIds = aws.StringSlice([]string{"sg-elsewhere"})
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.Empty(t, h.launch.enis.modified)
	assert.Equal(t, []string{testDefaultSG}, h.record(t).VpcSecurityGroupIDs)
}

// Re-sending the groups already attached is what Terraform does on every apply.
// It is not a change, so it must not reach the ENI at all.
func TestModifyDBInstance_IgnoresTheSecurityGroupsAlreadyAttached(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.VpcSecurityGroupIds = aws.StringSlice([]string{testDefaultSG})
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.NoError(t, err)
	assert.Empty(t, h.launch.enis.modified)
}

// Storage is grow-only, and a request to shrink must be refused before the
// instance moves anywhere — a rejected request cannot cost a running database
// its availability.
func TestModifyDBInstance_RejectsAStorageShrinkBeforeAnythingMoves(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifiableRecord()
	rec.AllocatedStorage = 100
	seedInstance(t, h.svc, rec)

	in := modifyInput()
	in.AllocatedStorage = aws.Int64(50)
	in.ApplyImmediately = aws.Bool(true)
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterCombination)
	assert.Empty(t, h.cmdr.calls)
	assert.Empty(t, h.storage.modified)

	stored := h.record(t)
	assert.Equal(t, StatusAvailable, stored.Status)
	assert.Equal(t, int64(100), stored.AllocatedStorage)
}

// Terraform sends the whole body on every apply, so a size that repeats the
// current one accompanies unrelated changes constantly. Failing it would fail
// every one of them; it contributes nothing to the resulting configuration instead.
func TestModifyDBInstance_TreatsTheCurrentStorageSizeAsNoChange(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.AllocatedStorage = aws.Int64(20)
	in.DBInstanceClass = aws.String("db.t3.medium")
	in.DeletionProtection = aws.Bool(true)
	in.ApplyImmediately = aws.Bool(true)
	out, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, string(StatusAvailable), aws.StringValue(out.DBInstance.DBInstanceStatus))
	assert.Empty(t, h.cmdr.calls, "repeating the current size and class is not an outage")
	assert.Empty(t, h.storage.modified)

	stored := h.record(t)
	assert.True(t, stored.DeletionProtection, "the change that was real still landed")
	assert.Nil(t, stored.PendingModifiedValues)
}

// A request that changes nothing at all is answered with the instance as it
// stands rather than an outage or an error.
func TestModifyDBInstance_IsANoOpWhenNothingDiffers(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.DBInstanceClass = aws.String("db.t3.medium")
	in.ApplyImmediately = aws.Bool(true)
	out, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.NoError(t, err)
	assert.Equal(t, string(StatusAvailable), aws.StringValue(out.DBInstance.DBInstanceStatus))
	assert.Empty(t, h.launch.launcher.terminated)
	assert.Nil(t, h.record(t).PendingModifiedValues)
}

// The db.* classes are a facade over the platform's instance types, so a
// class with nothing behind it is rejected with the set that has.
func TestModifyDBInstance_RejectsAnUnmappedInstanceClass(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.DBInstanceClass = aws.String("db.r5.24xlarge")
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.Contains(t, err.Error(), "db.t3.medium", "the rejection names the classes that do resolve")
	assert.Equal(t, StatusAvailable, h.record(t).Status)
}

// Until real groups are materialised the implicit default is the only name
// that resolves, so any other one names a group that does not exist.
func TestModifyDBInstance_RejectsAnUnknownParameterGroup(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.DBParameterGroupName = aws.String("tuned-for-oltp")
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBParameterGroupNotFound)
	assert.Nil(t, h.record(t).PendingModifiedValues)
}

func TestModifyDBInstance_RejectsAnIncompatibleParameterGroupBeforeMutation(t *testing.T) {
	h := newModifyHarness(t)
	createLargeMemoryParameterGroup(t, h)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.DBParameterGroupName = aws.String(largeMemoryParameterGroup)
	in.DeletionProtection = aws.Bool(true)
	in.ApplyImmediately = aws.Bool(true)
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	stored := h.record(t)
	assert.Equal(t, StatusAvailable, stored.Status)
	assert.Equal(t, testDefaultGroup, stored.DBParameterGroupName)
	assert.False(t, stored.DeletionProtection)
	assert.Nil(t, stored.PendingModifiedValues)
	assert.Empty(t, h.agent.received())
}

func TestModifyDBInstance_ValidatesClassChangeAgainstCurrentGroup(t *testing.T) {
	h := newModifyHarness(t)
	createLargeMemoryParameterGroup(t, h)
	rec := modifiableRecord()
	rec.DBInstanceClass = "db.m5.xlarge"
	rec.DBParameterGroupName = largeMemoryParameterGroup
	seedInstance(t, h.svc, rec)

	in := modifyInput()
	in.DBInstanceClass = aws.String("db.t3.medium")
	in.DeletionProtection = aws.Bool(true)
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	stored := h.record(t)
	assert.Equal(t, StatusAvailable, stored.Status)
	assert.Equal(t, "db.m5.xlarge", stored.DBInstanceClass)
	assert.False(t, stored.DeletionProtection)
	assert.Nil(t, stored.PendingModifiedValues)
}

func TestModifyDBInstance_ValidatesSimultaneousGroupAndClassAgainstTargets(t *testing.T) {
	h := newModifyHarness(t)
	createLargeMemoryParameterGroup(t, h)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.DBParameterGroupName = aws.String(largeMemoryParameterGroup)
	in.DBInstanceClass = aws.String("db.m5.xlarge")
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.NoError(t, err)
	stored := h.record(t)
	require.NotNil(t, stored.PendingModifiedValues)
	assert.Equal(t, largeMemoryParameterGroup, stored.PendingModifiedValues.DBParameterGroupName)
	assert.Equal(t, "db.m5.xlarge", stored.PendingModifiedValues.DBInstanceClass)
}

// These fields are stored rather than acted on, but a value outside AWS's
// range would fail in a maintenance window nobody is watching.
func TestModifyDBInstance_RejectsAnOutOfRangeBackupRetention(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.BackupRetentionPeriod = aws.Int64(400)
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
	assert.Zero(t, h.record(t).BackupRetentionPeriod)
}

// A supported action carrying a parameter this platform does not implement
// is rejected, never served with the parameter quietly dropped. Each of these
// would otherwise leave the customer believing in a guarantee they do not have.
func TestModifyDBInstance_RejectsTheUnimplementedParameters(t *testing.T) {
	cases := map[string]func(*rds.ModifyDBInstanceInput){
		"MultiAZ":                         func(in *rds.ModifyDBInstanceInput) { in.MultiAZ = aws.Bool(true) },
		"PubliclyAccessible":              func(in *rds.ModifyDBInstanceInput) { in.PubliclyAccessible = aws.Bool(true) },
		"NewDBInstanceIdentifier":         func(in *rds.ModifyDBInstanceInput) { in.NewDBInstanceIdentifier = aws.String("renamed") },
		"EngineVersion":                   func(in *rds.ModifyDBInstanceInput) { in.EngineVersion = aws.String("19.1") },
		"Engine":                          func(in *rds.ModifyDBInstanceInput) { in.Engine = aws.String("mysql") },
		"DBPortNumber":                    func(in *rds.ModifyDBInstanceInput) { in.DBPortNumber = aws.Int64(6543) },
		"DBSubnetGroupName":               func(in *rds.ModifyDBInstanceInput) { in.DBSubnetGroupName = aws.String("other-subnets") },
		"MaxAllocatedStorage":             func(in *rds.ModifyDBInstanceInput) { in.MaxAllocatedStorage = aws.Int64(500) },
		"Iops":                            func(in *rds.ModifyDBInstanceInput) { in.Iops = aws.Int64(3000) },
		"StorageThroughput":               func(in *rds.ModifyDBInstanceInput) { in.StorageThroughput = aws.Int64(250) },
		"StorageType":                     func(in *rds.ModifyDBInstanceInput) { in.StorageType = aws.String("io2") },
		"ManageMasterUserPassword":        func(in *rds.ModifyDBInstanceInput) { in.ManageMasterUserPassword = aws.Bool(true) },
		"EnableIAMDatabaseAuthentication": func(in *rds.ModifyDBInstanceInput) { in.EnableIAMDatabaseAuthentication = aws.Bool(true) },
		"DBSecurityGroups":                func(in *rds.ModifyDBInstanceInput) { in.DBSecurityGroups = aws.StringSlice([]string{"classic-sg"}) },
		"CACertificateIdentifier":         func(in *rds.ModifyDBInstanceInput) { in.CACertificateIdentifier = aws.String("rds-ca-2019") },
		"Domain":                          func(in *rds.ModifyDBInstanceInput) { in.Domain = aws.String("d-9876543210") },
		"OptionGroupName":                 func(in *rds.ModifyDBInstanceInput) { in.OptionGroupName = aws.String("custom-options") },
	}

	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := modifyInput()
			mutate(in)
			_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

			require.Error(t, err, "%s must be rejected rather than silently dropped", name)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
			assert.Equal(t, StatusAvailable, h.record(t).Status)
		})
	}
}

// gp3 is the only type offered, so naming it is not a change and must not be
// caught by the rejection that guards the types that are not offered.
func TestModifyDBInstance_AcceptsTheStorageTypeItAlreadyHas(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.StorageType = aws.String("GP3")
	in.DeletionProtection = aws.Bool(true)
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.NoError(t, err)
	assert.True(t, h.record(t).DeletionProtection)
}

// A disruptive change without ApplyImmediately is recorded and left for the
// maintenance window: the database keeps serving until then.
func TestModifyDBInstance_DefersADisruptiveChangeToTheMaintenanceWindow(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())

	in := modifyInput()
	in.AllocatedStorage = aws.Int64(50)
	in.DBInstanceClass = aws.String("db.m5.large")
	in.CopyTagsToSnapshot = aws.Bool(true)
	out, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, string(StatusAvailable), aws.StringValue(out.DBInstance.DBInstanceStatus))
	assert.True(t, aws.BoolValue(out.DBInstance.CopyTagsToSnapshot),
		"the response must include immediate fields applied alongside a deferred change")
	assert.Empty(t, h.cmdr.calls)
	assert.Empty(t, h.storage.modified)
	assert.Empty(t, h.launch.launcher.terminated)

	rec := h.record(t)
	assert.True(t, rec.CopyTagsToSnapshot)
	require.NotNil(t, rec.PendingModifiedValues)
	assert.Equal(t, int64(50), aws.Int64Value(rec.PendingModifiedValues.AllocatedStorage))
	assert.Equal(t, "db.m5.large", rec.PendingModifiedValues.DBInstanceClass)
	// Not yet in effect: reporting the new values as current would tell the
	// customer a change that has not happened has.
	assert.Equal(t, int64(20), rec.AllocatedStorage)
	assert.Equal(t, "db.t3.medium", rec.DBInstanceClass)

	// Reported to the customer, since the change they asked for is not the one
	// they got: it lands later, with an outage.
	require.NotEmpty(t, h.eventMessages(t))
	assert.Contains(t, h.eventMessages(t)[0], "maintenance window")
}

// A disruptive change needs a live engine to stop cleanly and a live agent to
// apply parameters, so it is legal only from a state that has both.
func TestModifyDBInstance_RejectsADisruptiveChangeWhileNotAvailable(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifiableRecord()
	rec.Status = StatusStopped
	seedInstance(t, h.svc, rec)

	in := modifyInput()
	in.AllocatedStorage = aws.Int64(50)
	in.ApplyImmediately = aws.Bool(true)
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceInvalidState)
	assert.Equal(t, StatusStopped, h.record(t).Status)
	assert.Nil(t, h.record(t).PendingModifiedValues)
}

// A failed instance is exactly the one a customer retries a change on, so the
// same change is accepted from there.
func TestModifyDBInstance_AcceptsARetryFromFailed(t *testing.T) {
	h := newModifyHarness(t)
	rec := modifiableRecord()
	rec.Status = StatusFailed
	rec.FailureReason = "the DB instance could not be modified"
	seedInstance(t, h.svc, rec)

	in := modifyInput()
	in.AllocatedStorage = aws.Int64(50)
	in.ApplyImmediately = aws.Bool(true)
	out, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)

	require.NoError(t, err)
	assert.Equal(t, string(StatusModifying), aws.StringValue(out.DBInstance.DBInstanceStatus))
}

// The whole immediate path: the engine is stopped cleanly, the VM goes down,
// the volume grows, the VM comes back, and the instance stays in modifying with
// the in-guest filesystem grow still outstanding.
func TestModifyDBInstance_AppliesAStorageGrowImmediately(t *testing.T) {
	h := newModifyHarness(t)
	seed := modifiableRecord()
	seed.FormatAuthorized = true
	seedInstance(t, h.svc, seed)

	in := modifyInput()
	in.AllocatedStorage = aws.Int64(50)
	in.ApplyImmediately = aws.Bool(true)
	out, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, string(StatusModifying), aws.StringValue(out.DBInstance.DBInstanceStatus))
	assert.Equal(t, []string{"stop:" + testInstance, "start:" + testInstance}, h.cmdr.calls)
	require.Len(t, h.storage.modified, 1)
	assert.Equal(t, int64(50), aws.Int64Value(h.storage.modified[0].Size))

	issued := h.agent.received()
	require.Len(t, issued, 1, "the engine is checkpointed before its VM stops")
	assert.Equal(t, CommandStopEngine, issued[0].Type)

	rec := h.record(t)
	assert.Equal(t, int64(50), rec.AllocatedStorage)
	assert.Equal(t, testInstance, rec.InstanceID, "a grow restarts the VM rather than replacing it")
	assert.False(t, rec.FormatAuthorized, "storage grow must not carry create-time formatting into the restart")
	require.NotNil(t, rec.PendingModifiedValues)
	assert.True(t, rec.PendingModifiedValues.FilesystemGrowPending)
	assert.Nil(t, rec.PendingModifiedValues.AllocatedStorage, "the volume is at its new size")
}

// A change that cannot be delivered leaves the instance failed with the reason
// and the request still recorded, so the reconciler or the customer can retry
// it rather than having to reconstruct what was asked for.
func TestModifyDBInstance_FailsTheInstanceAndKeepsThePendingValues(t *testing.T) {
	h := newModifyHarness(t)
	seedInstance(t, h.svc, modifiableRecord())
	h.storage.modifyErr = errors.New("the volume store rejected the resize")

	in := modifyInput()
	in.AllocatedStorage = aws.Int64(50)
	in.ApplyImmediately = aws.Bool(true)
	_, err := h.svc.ModifyDBInstance(t.Context(), in, testAccountID)
	require.Error(t, err)

	rec := h.record(t)
	assert.Equal(t, StatusFailed, rec.Status)
	assert.Contains(t, rec.FailureReason, "could not be modified")
	require.NotNil(t, rec.PendingModifiedValues)
	assert.Equal(t, int64(50), aws.Int64Value(rec.PendingModifiedValues.AllocatedStorage))
	assert.Equal(t, int64(20), rec.AllocatedStorage, "the record reports the size the volume actually has")
	assert.Equal(t, []string{"stop:" + testInstance, "start:" + testInstance}, h.cmdr.calls)

	messages := h.eventMessages(t)
	assert.Contains(t, messages, "DB instance restarted after its storage grow failed; storage is unchanged.")
	assert.Contains(t, messages, "the DB instance could not be modified: grow the data volume vol-rdsdata01 to 50 GiB: the volume store rejected the resize")
}

func TestModifyDBInstance_RequiresAnIdentifier(t *testing.T) {
	h := newModifyHarness(t)

	_, err := h.svc.ModifyDBInstance(t.Context(), &rds.ModifyDBInstanceInput{}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)

	_, err = h.svc.ModifyDBInstance(t.Context(), nil, testAccountID)
	require.Error(t, err)
}

func TestModifyDBInstance_RejectsAnUnknownInstance(t *testing.T) {
	h := newModifyHarness(t)

	_, err := h.svc.ModifyDBInstance(t.Context(), modifyInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceNotFound)
}

// The event ring's messages, newest first, which is where a deferred change is
// reported to the customer.
func (h *modifyHarness) eventMessages(t *testing.T) []string {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var ring eventRing
	found, err := getJSON(t.Context(), kv, EventRingKey(EventSourceTypeDBInstance, testDBID), &ring)
	require.NoError(t, err)
	if !found {
		return nil
	}
	messages := make([]string, 0, len(ring.Events))
	for _, event := range slices.Backward(ring.Events) {
		messages = append(messages, event.Message)
	}
	return messages
}
