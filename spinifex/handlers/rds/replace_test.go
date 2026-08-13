package handlers_rds

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The instance ID fakeLauncher hands back, which is what a replace has to leave
// the record and the index naming.
const testReplacementInstance = "i-rds0001"

// Puts the record in KV along with the instance-index entry the current VM's
// agent authenticates through, which is the pair a replace has to move
// together.
func seedReplaceable(t *testing.T, h *modifyHarness, rec DBInstanceRecord) DBInstanceRecord {
	t.Helper()
	seedInstance(t, h.svc, rec)
	require.NoError(t, h.svc.PutInstanceIndex(t.Context(), rec.InstanceID, InstanceIndexEntry{
		AccountID:            testAccountID,
		DBInstanceIdentifier: rec.DBInstanceIdentifier,
		VMGeneration:         rec.VMGeneration,
	}))
	return rec
}

func TestReplaceInstanceVM_StopsBeforeDestructiveWorkWhenCancelled(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errModifyLeaseLost)

	err := h.svc.replaceInstanceVM(ctx, h.kv(t), testAccountID, &rec, replaceInput{Reason: "recovery"})
	require.ErrorIs(t, err, errModifyLeaseLost)
	assert.Empty(t, h.cmdr.calls)
	assert.Empty(t, h.launch.launcher.terminated)
	assert.Nil(t, h.launch.launcher.input)
}

// The endpoint ENI and the datadir outlive the VM, so a replace adopts
// both. Minting either would move the address clients resolve or hand the
// customer an empty database.
func TestReplaceInstanceVM_ReusesTheEndpointENIAndDataVolume(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())

	require.NoError(t, h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec,
		replaceInput{InstanceClass: "db.m5.large", InstanceType: "m5.large", Reason: "the instance class changed"}))

	require.NotNil(t, h.launch.launcher.input)
	require.Len(t, h.launch.launcher.input.ExtraENIs, 1)
	endpoint := h.launch.launcher.input.ExtraENIs[0]
	assert.Equal(t, testEndpointENI, endpoint.ENIID)
	assert.Equal(t, testEndpointIP, endpoint.ENIIP)
	assert.Equal(t, testEndpointMAC, endpoint.ENIMac, "the replacement NIC comes up with the endpoint's own MAC")
	assert.Equal(t, "m5.large", h.launch.launcher.input.InstanceType)

	// The stale attachment is cleared first: re-attaching an ENI that still
	// reads as attached is rejected.
	assert.Contains(t, h.launch.enis.detached, testEndpointENI)
	assert.Len(t, h.launch.enis.created, 1, "only the disposable system NIC is minted")
	assert.NotContains(t, h.launch.enis.deleted, testEndpointENI)

	assert.Empty(t, h.launch.volumes.created, "the datadir is re-attached, not recreated")
	assert.Equal(t, testDataVolume, h.launch.attacher.volumeID)
	assert.Empty(t, h.launch.volumes.deleted)
}

// The engine is checkpointed and the old VM is gone before the new one comes
// up, or two VMs hold the same datadir.
func TestReplaceInstanceVM_IAMFailureLeavesTheCurrentVMServing(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())
	iamErr := errors.New("IAM store unavailable")
	h.iam.PutRolePolicyErr = iamErr

	err := h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{
		GrowStorageToGiB: 80,
		Reason:           "the instance class changed",
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, iamErr)
	assert.Empty(t, h.agent.received())
	assert.Empty(t, h.launch.launcher.terminated)
	assert.Empty(t, h.launch.enis.deleted)
	assert.Empty(t, h.storage.modified)
	assert.Nil(t, h.launch.launcher.input)
	assert.Equal(t, testInstance, h.record(t).InstanceID)
}

func TestReplaceInstanceVM_StopsTheEngineAndTerminatesTheOldVMFirst(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())

	var terminatedAtLaunch []string
	h.launch.launcher.onLaunch = func() {
		terminatedAtLaunch = append([]string(nil), h.launch.launcher.terminated...)
	}

	require.NoError(t, h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{Reason: "recovery"}))

	assert.Equal(t, []string{testInstance}, terminatedAtLaunch)
	issued := h.agent.received()
	require.Len(t, issued, 1)
	assert.Equal(t, CommandStopEngine, issued[0].Type)

	// The old system NIC is disposable, and leaving it behind would hold an
	// address in the shared RDS system subnet.
	assert.Contains(t, h.launch.enis.deleted, "eni-sys01")
}

// While the old index entry exists the superseded agent's IMDS credentials
// still resolve to this DB instance, so it goes before the new one lands.
func TestReplaceInstanceVM_RewritesTheInstanceIndex(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())

	require.NoError(t, h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{Reason: "recovery"}))

	superseded, err := h.svc.LookupInstanceIndex(t.Context(), testInstance)
	require.NoError(t, err)
	assert.Nil(t, superseded, "the superseded VM must no longer resolve to this DB instance")

	current, err := h.svc.LookupInstanceIndex(t.Context(), testReplacementInstance)
	require.NoError(t, err)
	require.NotNil(t, current)
	assert.Equal(t, testDBID, current.DBInstanceIdentifier)
	assert.Equal(t, testAccountID, current.AccountID)
	assert.Equal(t, int64(2), current.VMGeneration)
}

// The record names the new VM and forgets the old one's health, while every
// field the endpoint is built from is left exactly as it was.
func TestReplaceInstanceVM_RecordsTheNewVMAndKeepsTheEndpoint(t *testing.T) {
	h := newModifyHarness(t)
	seed := modifiableRecord()
	seed.FormatAuthorized = true
	beat := time.Now().UTC()
	seed.Agent = AgentState{InstanceID: testInstance, EngineHealth: EngineHealthHealthy, LastSeen: &beat}
	rec := seedReplaceable(t, h, seed)

	require.NoError(t, h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{Reason: "recovery"}))

	stored := h.record(t)
	assert.Equal(t, testReplacementInstance, stored.InstanceID)
	assert.Equal(t, int64(2), stored.VMGeneration)
	assert.NotEqual(t, "eni-sys01", stored.SystemENIID)
	// The old VM's health must not read as the new one's, or the reconciler
	// calls the replace finished before the replacement has said anything.
	assert.Empty(t, stored.Agent.InstanceID)
	assert.Nil(t, stored.Agent.LastSeen)

	assert.Equal(t, testEndpointENI, stored.ENIID)
	assert.Equal(t, seed.ENIPrivateIP, stored.ENIPrivateIP)
	assert.Equal(t, seed.DNSName, stored.DNSName)
	assert.Equal(t, testDataVolume, stored.DataVolumeID)
	assert.Equal(t, "volrdsdata01", stored.DataVolumeSerial)
	assert.False(t, stored.FormatAuthorized, "replacement must revoke an initial-create grant")

	// The caller's copy is kept in step, so its own record write cannot
	// resurrect the old VM's identity on top of this one.
	assert.Equal(t, testReplacementInstance, rec.InstanceID)
	assert.Equal(t, int64(2), rec.VMGeneration)
	assert.False(t, rec.FormatAuthorized)
}

// A grow riding a class change takes the only window in which nothing holds the
// volume: after the old VM is gone and before the new one exists.
func TestReplaceInstanceVM_GrowsTheVolumeWhileNoVMHoldsIt(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())

	var terminatedAtGrow int
	var launchedAtGrow bool
	h.storage.onModify = func() {
		terminatedAtGrow = len(h.launch.launcher.terminated)
		launchedAtGrow = h.launch.launcher.input != nil
	}

	require.NoError(t, h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{
		InstanceClass: "db.m5.large", InstanceType: "m5.large", GrowStorageToGiB: 80, Reason: "the instance class changed",
	}))

	assert.Equal(t, 1, terminatedAtGrow, "the volume was grown while the old VM still held it")
	assert.False(t, launchedAtGrow, "the volume was grown after the replacement had already attached it")
	assert.Equal(t, int64(80), h.storage.sizes[testDataVolume])
}

// An unmapped class has no instance type behind it, so the replace is refused
// before the VM it would land on is torn down.
func TestReplaceInstanceVM_RefusesAnUnmappedRecordedClass(t *testing.T) {
	h := newModifyHarness(t)
	seed := modifiableRecord()
	seed.DBInstanceClass = "db.r5.24xlarge"
	rec := seedReplaceable(t, h, seed)

	err := h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{Reason: "recovery"})
	require.Error(t, err)
	assert.Empty(t, h.launch.launcher.terminated)
}

// Without the persisted ENI and volume there is nothing to replace *onto*: a
// launch from here would mint a new address and an empty datadir.
func TestReplaceInstanceVM_RefusesWithoutThePersistedIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*DBInstanceRecord){
		"no endpoint ENI": func(rec *DBInstanceRecord) { rec.ENIID = "" },
		"no data volume":  func(rec *DBInstanceRecord) { rec.DataVolumeID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newModifyHarness(t)
			seed := modifiableRecord()
			mutate(&seed)
			rec := seedReplaceable(t, h, seed)

			err := h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{Reason: "recovery"})
			require.Error(t, err)
			assert.Empty(t, h.launch.launcher.terminated)
			assert.Nil(t, h.launch.launcher.input)
		})
	}
}

// An endpoint ENI that no longer exists is an error, not an invitation to mint
// a replacement: a new ENI would come up on a different address than the one
// DNS and the serving certificate name.
func TestReplaceInstanceVM_RefusesToMintAReplacementEndpoint(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())
	h.launch.enis.describeMissing = true

	err := h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{Reason: "recovery"})
	require.Error(t, err)
	assert.Nil(t, h.launch.launcher.input)
	assert.Equal(t, testInstance, h.record(t).InstanceID)
}

// Every step that can fail leaves the record still naming the VM and generation
// it had, so the reconciler resumes from a state it can read rather than from
// one that half-describes two VMs.
func TestReplaceInstanceVM_LeavesTheRecordRecoverableOnEveryFailure(t *testing.T) {
	cases := map[string]func(*modifyHarness){
		"the old VM will not terminate": func(h *modifyHarness) {
			h.launch.launcher.terminateErr = errors.New("the node did not answer")
		},
		"the volume will not grow": func(h *modifyHarness) {
			h.storage.modifyErr = errors.New("the volume store is unavailable")
		},
		"the replacement will not launch": func(h *modifyHarness) {
			h.launch.launcher.err = errors.New("no capacity on any node")
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			h := newModifyHarness(t)
			rec := seedReplaceable(t, h, modifiableRecord())
			breakIt(h)

			err := h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec,
				replaceInput{GrowStorageToGiB: 80, Reason: "the instance class changed"})
			require.Error(t, err)

			stored := h.record(t)
			assert.Equal(t, testInstance, stored.InstanceID)
			assert.Equal(t, int64(firstVMGeneration), stored.VMGeneration)
			assert.Equal(t, testEndpointENI, stored.ENIID)
			assert.Equal(t, testDataVolume, stored.DataVolumeID)

			// The index still resolves the VM the record names, so an agent that
			// is somehow still alive is not locked out of a control plane that
			// has not moved on.
			entry, err := h.svc.LookupInstanceIndex(t.Context(), testInstance)
			require.NoError(t, err)
			require.NotNil(t, entry)
			assert.Equal(t, testDBID, entry.DBInstanceIdentifier)
		})
	}
}

// A failed launch tears down what it created rather than leaving the DB
// instance's own ENI and volume attached to a VM nobody will ever record.
func TestReplaceInstanceVM_UnwindsAFailedLaunchWithoutTouchingTheEndpoint(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())
	h.launch.launcher.err = errors.New("no capacity on any node")

	require.Error(t, h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{Reason: "recovery"}))

	assert.NotContains(t, h.launch.enis.deleted, testEndpointENI, "the endpoint ENI must survive a failed replace")
	assert.Empty(t, h.launch.volumes.deleted, "the datadir must survive a failed replace")
}

// Auto-recovery replaces onto the sizing the record already carries, so an
// empty class in the request is not a class change.
func TestReplaceInstanceVM_KeepsTheRecordedSizingWhenNoneIsRequested(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())

	require.NoError(t, h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{Reason: "recovery"}))

	require.NotNil(t, h.launch.launcher.input)
	assert.Equal(t, "t3.medium", h.launch.launcher.input.InstanceType)
	assert.Equal(t, "db.t3.medium", h.record(t).DBInstanceClass)
	assert.Empty(t, h.storage.modified, "a replace with no grow requested does not touch the volume")
}

// The customer's own event ring is where a replace becomes attributable to the
// change or the failure that caused it.
func TestReplaceInstanceVM_RecordsWhyTheVMWasReplaced(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())

	require.NoError(t, h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec,
		replaceInput{Reason: "the instance class changed to db.m5.large"}))

	messages := h.eventMessages(t)
	require.NotEmpty(t, messages)
	assert.Contains(t, messages[0], "db.m5.large")
}

// The replacement agent's first bootstrap must be an attach, not an
// initialize: the datadir is already there, and a second initdb would be
// destructive. The user data is what carries the identity it fetches with.
func TestReplaceInstanceVM_LaunchesWithTheInstancesOwnIdentity(t *testing.T) {
	h := newModifyHarness(t)
	rec := seedReplaceable(t, h, modifiableRecord())

	require.NoError(t, h.svc.replaceInstanceVM(t.Context(), h.kv(t), testAccountID, &rec, replaceInput{Reason: "recovery"}))

	require.NotNil(t, h.launch.launcher.input)
	assert.Contains(t, h.launch.launcher.input.UserData, testDBID)
	assert.Equal(t, testDBSubnet, h.launch.launcher.input.ExtraENIs[0].SubnetID)
	assert.Equal(t, testAccountID, h.launch.launcher.input.ExtraENIs[0].AccountID)
	assert.NotEqual(t, testAccountID, h.launch.launcher.input.AccountID,
		"the VM and its management NIC live in the system account")
	assert.Equal(t, rdsInstanceProfileARN(utils.GlobalAccountID), h.launch.launcher.input.IamInstanceProfileArn)
}
