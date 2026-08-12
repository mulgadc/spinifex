package handlers_rds

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/rds"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	iammock "github.com/mulgadc/spinifex/spinifex/handlers/iam/mock"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// snapshotHarness is a Service wired for both halves of this phase: the EC2
// snapshot and volume services a create and a delete drive, and the customer
// VPC and launch fakes a restore needs on top.
type snapshotHarness struct {
	svc     *Service
	launch  *launchHarness
	network *fakeNetwork
	iam     *iammock.SystemInstanceRoleEnsurer
	snaps   *fakeSnapshots
	agent   *stubAgent
	nc      *nats.Conn
}

func newSnapshotHarness(t *testing.T, agentFails bool) *snapshotHarness {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	h := &snapshotHarness{
		launch:  newLaunchHarness(),
		network: newFakeNetwork(),
		iam:     iammock.New(),
		snaps:   &fakeSnapshots{},
		nc:      nc,
	}
	h.launch.volumes.encrypted = true
	h.agent = newStubAgent(t, nc, testAccountID, testDBID, agentFails)
	// Without a responder the best-effort endpoint publish would sit out its own
	// timeout on every restore.
	stubDNSWriter(t, nc)

	h.svc = NewService(nc, testRegion).WithDeps(Deps{
		LoadCA:    newTestCA(t),
		MasterKey: testMasterKey,
		Launch:    h.launch.deps(),
		Network:   h.network,
		IAM:       testIAMProvider(h.iam),
		Snapshots: h.snaps,
	})
	return h
}

func stubDNSWriter(t *testing.T, nc *nats.Conn) {
	t.Helper()
	sub, err := nc.Subscribe(handlers_dns.SubjectRecordsetChange, func(msg *nats.Msg) {
		if err := msg.Respond([]byte(`{}`)); err != nil {
			t.Logf("respond on %s: %v", handlers_dns.SubjectRecordsetChange, err)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func (h *snapshotHarness) instance(t *testing.T, id string) DBInstanceRecord {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBInstanceRecord
	found, err := getJSON(t.Context(), kv, DBInstanceKey(id), &rec)
	require.NoError(t, err)
	require.True(t, found, "DB instance %s should be stored", id)
	return rec
}

func (h *snapshotHarness) snapshot(t *testing.T, id string) (DBSnapshotRecord, bool) {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec DBSnapshotRecord
	found, err := getJSON(t.Context(), kv, DBSnapshotKey(id), &rec)
	require.NoError(t, err)
	return rec, found
}

func (h *snapshotHarness) events(t *testing.T, sourceType, id string) []Event {
	t.Helper()
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var ring eventRing
	found, err := getJSON(t.Context(), kv, EventRingKey(sourceType, id), &ring)
	require.NoError(t, err)
	if !found {
		return nil
	}
	return ring.Events
}

// seedSnapshotSource stores an available instance with a data volume, carrying
// the configuration a restore has to be able to reproduce from the snapshot
// alone.
func (h *snapshotHarness) seedSnapshotSource(t *testing.T) DBInstanceRecord {
	t.Helper()
	rec := availableRecord()
	rec.DBInstanceClass = "db.t3.medium"
	rec.AllocatedStorage = 20
	rec.StorageType = storageTypeGP3
	rec.StorageEncrypted = true
	rec.VpcID = testDefaultVPC
	rec.VpcSecurityGroupIDs = []string{testDefaultSG}
	seedInstance(t, h.svc, rec)
	return rec
}

const testSnapshotID = "orders-db-pre-upgrade"

func snapshotInput() *rds.CreateDBSnapshotInput {
	return &rds.CreateDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(testSnapshotID),
		DBInstanceIdentifier: aws.String(testDBID),
	}
}

func TestCreateDBSnapshot_QuiescesTheEngineAndRecordsTheSnapshot(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)

	out, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.NoError(t, err)

	require.NotNil(t, out.DBSnapshot)
	assert.Equal(t, testSnapshotID, aws.StringValue(out.DBSnapshot.DBSnapshotIdentifier))
	assert.Equal(t, SnapshotStatusAvailable, aws.StringValue(out.DBSnapshot.Status))
	assert.Equal(t, SnapshotTypeManual, aws.StringValue(out.DBSnapshot.SnapshotType))
	assert.Equal(t, DBSnapshotARN(testRegion, testAccountID, testSnapshotID),
		aws.StringValue(out.DBSnapshot.DBSnapshotArn))

	// The engine is held at a checkpoint for the length of the EC2 call and let
	// go again, so the captured datadir is a checkpoint rather than a mid-write
	// state.
	issued := make([]string, 0, 2)
	for _, cmd := range h.agent.received() {
		issued = append(issued, cmd.Type)
	}
	assert.Equal(t, []string{CommandQuiesce, CommandUnquiesce}, issued)

	require.Len(t, h.snaps.created, 1)
	assert.Equal(t, "vol-rdsdata01", aws.StringValue(h.snaps.created[0].VolumeId))
	assert.Equal(t, tags.ManagedByRDS, tagOf(h.snaps.created[0].TagSpecifications, tags.ManagedByKey))
	assert.Equal(t, testSnapshotID, tagOf(h.snaps.created[0].TagSpecifications, rdsSnapshotTagKey))
	assert.Equal(t, testAccountID, tagOf(h.snaps.created[0].TagSpecifications, rdsSnapshotAccountTagKey))

	stored, found := h.snapshot(t, testSnapshotID)
	require.True(t, found)
	assert.Equal(t, "snap-0001", stored.SnapshotID)
	assert.False(t, stored.CrashConsistent)
	// Everything a restore needs is copied, because the instance it describes may
	// be gone by the time the snapshot is used.
	assert.Equal(t, "vol-rdsdata01", stored.SourceVolumeID)
	assert.Equal(t, "db.t3.medium", stored.DBInstanceClass)
	assert.Equal(t, "orders", stored.DBName)
	assert.Equal(t, int64(5432), stored.Port)
}

// The hold has to be released before the instance settles, so the deadline the
// agent enforces is never what ends a successful snapshot.
func TestCreateDBSnapshot_ReturnsTheInstanceToWhereItWasFound(t *testing.T) {
	for _, resume := range []Status{StatusAvailable, StatusStopped} {
		t.Run(string(resume), func(t *testing.T) {
			h := newSnapshotHarness(t, false)
			rec := availableRecord()
			rec.Status = resume
			seedInstance(t, h.svc, rec)

			_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
			require.NoError(t, err)

			stored := h.instance(t, testDBID)
			assert.Equal(t, resume, stored.Status)
			assert.Nil(t, stored.SnapshotOperation)
			assert.Nil(t, stored.TransitionStartedAt)
		})
	}
}

// A stopped instance's datadir was sealed by the graceful stop, which is the
// checkpoint a quiesce would be forcing — so there is no engine to hold.
func TestCreateDBSnapshot_DoesNotQuiesceAStoppedInstance(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := availableRecord()
	rec.Status = StatusStopped
	seedInstance(t, h.svc, rec)

	_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.NoError(t, err)

	assert.Empty(t, h.agent.received(), "a stopped engine has nothing to quiesce")
	stored, _ := h.snapshot(t, testSnapshotID)
	assert.False(t, stored.CrashConsistent)
}

// A snapshot that could not be quiesced is still a restorable backup, so it
// is taken and reported honestly rather than refused.
func TestCreateDBSnapshot_FallsBackToCrashConsistentWhenTheQuiesceFails(t *testing.T) {
	h := newSnapshotHarness(t, true)
	h.seedSnapshotSource(t)

	out, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.NoError(t, err)
	assert.Equal(t, SnapshotStatusAvailable, aws.StringValue(out.DBSnapshot.Status))

	stored, _ := h.snapshot(t, testSnapshotID)
	assert.True(t, stored.CrashConsistent)
	require.Len(t, h.snaps.created, 1)

	// The customer is told, because a crash-consistent snapshot recovers rather
	// than restores instantly.
	assert.Contains(t, eventMessages(h.events(t, EventSourceTypeDBInstance, testDBID)),
		"The database engine could not be quiesced before the snapshot; the snapshot is crash consistent and will recover from its write-ahead log when it is restored.")
}

// The record only ever described a snapshot that now does not exist, so it goes
// with it rather than holding the identifier for a reconciler to puzzle over.
func TestCreateDBSnapshot_WithdrawsTheRecordWhenTheSnapshotFails(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)
	h.snaps.createErr = awserrors.Errorf(awserrors.ErrorInternalError, "the volume store refused the snapshot")

	_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.Error(t, err)

	_, found := h.snapshot(t, testSnapshotID)
	assert.False(t, found, "a snapshot that was never cut must not hold its identifier")
	assert.Equal(t, StatusAvailable, h.instance(t, testDBID).Status)
}

func TestCreateDBSnapshot_RejectsATakenIdentifier(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)
	_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.NoError(t, err)

	_, err = h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBSnapshotAlreadyExists)
	// Checked before the instance is moved, so a duplicate leaves it where it was.
	assert.Equal(t, StatusAvailable, h.instance(t, testDBID).Status)
}

// One snapshot per instance at a time: the CAS into backing-up is the guard, so
// an instance already there is refused rather than entering the same window.
func TestCreateDBSnapshot_RefusesAnInstanceAlreadyBackingUp(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := availableRecord()
	rec.Status = StatusBackingUp
	rec.SnapshotOperation = &SnapshotOperation{
		DBSnapshotIdentifier: "orders-db-nightly",
		ResumeStatus:         StatusAvailable,
		StartedAt:            time.Now().UTC(),
	}
	seedInstance(t, h.svc, rec)

	_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBInstanceInvalidState)
	assert.Empty(t, h.snaps.created)
}

func TestCreateDBSnapshot_RejectsAnUnusableIdentifier(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)

	// Rejected because a name that cannot also be a FinalDBSnapshotIdentifier at
	// delete would make the two paths disagree.
	for _, id := range []string{"", "9-leading-digit", "trailing-", "double--hyphen", "UPPER"} {
		input := snapshotInput()
		input.DBSnapshotIdentifier = aws.String(id)
		_, err := h.svc.CreateDBSnapshot(t.Context(), input, testAccountID)
		require.Error(t, err, "identifier %q", id)
		assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue, "identifier %q", id)
	}
}

func TestDescribeDBSnapshots_FiltersByInstanceAndType(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)
	_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.NoError(t, err)

	// A snapshot of another instance, which the instance filter must exclude. It
	// is stopped so the snapshot needs no quiesce: only testDBID has an agent.
	other := availableRecord()
	other.DBInstanceIdentifier = "reports-db"
	other.Status = StatusStopped
	seedInstance(t, h.svc, other)
	second := snapshotInput()
	second.DBSnapshotIdentifier = aws.String("reports-db-daily")
	second.DBInstanceIdentifier = aws.String("reports-db")
	_, err = h.svc.CreateDBSnapshot(t.Context(), second, testAccountID)
	require.NoError(t, err)

	all, err := h.svc.DescribeDBSnapshots(t.Context(), &rds.DescribeDBSnapshotsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, all.DBSnapshots, 2)

	byInstance, err := h.svc.DescribeDBSnapshots(t.Context(), &rds.DescribeDBSnapshotsInput{
		DBInstanceIdentifier: aws.String(testDBID),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, byInstance.DBSnapshots, 1)
	assert.Equal(t, testSnapshotID, aws.StringValue(byInstance.DBSnapshots[0].DBSnapshotIdentifier))

	byType, err := h.svc.DescribeDBSnapshots(t.Context(), &rds.DescribeDBSnapshotsInput{
		SnapshotType: aws.String(SnapshotTypeAutomated),
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, byType.DBSnapshots, "nothing in this phase is automated")
}

// A client polling a create would otherwise read "gone" for "not ready".
func TestDescribeDBSnapshots_NamedSnapshotThatDoesNotExistIsAnError(t *testing.T) {
	h := newSnapshotHarness(t, false)

	_, err := h.svc.DescribeDBSnapshots(t.Context(), &rds.DescribeDBSnapshotsInput{
		DBSnapshotIdentifier: aws.String(testSnapshotID),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBSnapshotNotFound)
}

// A filter this phase cannot honour is rejected, because a silently
// unfiltered list reads as a complete answer.
func TestDescribeDBSnapshots_RejectsUnhonouredScoping(t *testing.T) {
	h := newSnapshotHarness(t, false)

	cases := map[string]*rds.DescribeDBSnapshotsInput{
		"Filters":       {Filters: []*rds.Filter{{Name: aws.String("engine")}}},
		"DbiResourceId": {DbiResourceId: aws.String("db-ABC123")},
		"IncludeShared": {IncludeShared: aws.Bool(true)},
		"SnapshotType":  {SnapshotType: aws.String("shared")},
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := h.svc.DescribeDBSnapshots(t.Context(), input, testAccountID)
			require.Error(t, err)
		})
	}
}

func TestDeleteDBSnapshot_RemovesTheSnapshotAndItsRecord(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)
	_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.NoError(t, err)

	out, err := h.svc.DeleteDBSnapshot(t.Context(), &rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(testSnapshotID),
	}, testAccountID)
	require.NoError(t, err)

	// AWS answers with the snapshot as it last stood, the way a DB instance
	// delete answers with the record it just removed.
	require.NotNil(t, out.DBSnapshot)
	assert.Equal(t, testSnapshotID, aws.StringValue(out.DBSnapshot.DBSnapshotIdentifier))

	assert.Equal(t, []string{"snap-0001"}, h.snaps.deleted)
	_, found := h.snapshot(t, testSnapshotID)
	assert.False(t, found)
	// The instance still owns its data volume, so nothing releases it here.
	assert.Empty(t, h.launch.volumes.deleted)
}

// A COW snapshot references its source volume's chunks, so a deleted instance's
// volume is retained until the last snapshot holding it goes.
func TestDeleteDBSnapshot_ReleasesARetainedVolumeWhenItWasTheLastHolder(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)
	_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.NoError(t, err)

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	retained := RetainedVolumeRecord{
		VolumeID:             "vol-rdsdata01",
		DBInstanceIdentifier: testDBID,
		Snapshots:            []string{"snap-0001"},
		RetainedAt:           time.Now().UTC(),
	}
	require.NoError(t, putJSON(t.Context(), kv, RetainedVolumeKey("vol-rdsdata01"), &retained))

	_, err = h.svc.DeleteDBSnapshot(t.Context(), &rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(testSnapshotID),
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, []string{"vol-rdsdata01"}, h.launch.volumes.deleted)
	var stillRetained RetainedVolumeRecord
	found, err := getJSON(t.Context(), kv, RetainedVolumeKey("vol-rdsdata01"), &stillRetained)
	require.NoError(t, err)
	assert.False(t, found, "the retained-volume record goes with the volume")
}

// The volume store's own index is the authority, not the list the retained
// record was written with: another snapshot taken since would otherwise read as
// "nothing holds it".
func TestDeleteDBSnapshot_KeepsARetainedVolumeAnotherSnapshotStillHolds(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)
	_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.NoError(t, err)

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	retained := RetainedVolumeRecord{
		VolumeID:          "vol-rdsdata01",
		Snapshots:         []string{"snap-0001"},
		HoldersUnresolved: true,
		RetainedAt:        time.Now().UTC(),
	}
	require.NoError(t, putJSON(t.Context(), kv, RetainedVolumeKey("vol-rdsdata01"), &retained))
	// A holder the record does not name, taken after it was written.
	h.snaps.holding = append(h.snaps.holding, "snap-9999")

	_, err = h.svc.DeleteDBSnapshot(t.Context(), &rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(testSnapshotID),
	}, testAccountID)
	require.NoError(t, err)

	assert.Empty(t, h.launch.volumes.deleted, "a volume another snapshot reads through must not go")
	var stillRetained RetainedVolumeRecord
	found, err := getJSON(t.Context(), kv, RetainedVolumeKey("vol-rdsdata01"), &stillRetained)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []string{"snap-9999"}, stillRetained.Snapshots)
	assert.False(t, stillRetained.HoldersUnresolved, "the holders are resolved now")
}

// The raw EC2 code says nothing a customer can act on, so the fault names the
// restored instance they have to remove first.
func TestDeleteDBSnapshot_NamesTheRestoredInstanceStillReadingFromIt(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)
	_, err := h.svc.CreateDBSnapshot(t.Context(), snapshotInput(), testAccountID)
	require.NoError(t, err)

	restored := availableRecord()
	restored.DBInstanceIdentifier = "orders-db-restored"
	restored.RestoredFromDBSnapshot = testSnapshotID
	seedInstance(t, h.svc, restored)
	h.snaps.deleteErr = awserrors.Errorf(awserrors.ErrorInvalidSnapshotInUse, "snap-0001 is in use")

	_, err = h.svc.DeleteDBSnapshot(t.Context(), &rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(testSnapshotID),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBSnapshotInvalidState)
	assert.Contains(t, err.Error(), "orders-db-restored")

	_, found := h.snapshot(t, testSnapshotID)
	assert.True(t, found, "a refused delete leaves the snapshot addressable")
}

// A snapshot still being taken has no data to remove and an in-flight writer
// that would recreate the record.
func TestDeleteDBSnapshot_RefusesASnapshotStillBeingTaken(t *testing.T) {
	h := newSnapshotHarness(t, false)
	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	creating := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
		CreatedAt:            time.Now().UTC(),
	}
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &creating))

	_, err = h.svc.DeleteDBSnapshot(t.Context(), &rds.DeleteDBSnapshotInput{
		DBSnapshotIdentifier: aws.String(testSnapshotID),
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), awserrors.ErrorDBSnapshotInvalidState)
}

// A snapshot still being taken has no PercentProgress, because reporting full
// progress would have a client restore from something that does not exist.
func TestProjectDBSnapshot_ReportsProgressOnlyWhenTheDataExists(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
	}
	assert.Nil(t, h.svc.projectDBSnapshot(&rec).PercentProgress)

	rec.Status = SnapshotStatusAvailable
	assert.Equal(t, int64(100), aws.Int64Value(h.svc.projectDBSnapshot(&rec).PercentProgress))
}

// The EC2 snapshot is tagged with the DB snapshot identifier before the record
// is flipped, so its presence is what says the data exists.
func TestReconciler_AdoptsTheEC2SnapshotOfAnUnfinishedDBSnapshot(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := NewReconciler(h.svc, "node-a")

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	stale := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
		CreatedAt:            time.Now().UTC().Add(-2 * snapshotResolveTimeout),
	}
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &stale))
	// The snapshot the dead worker cut, carrying the identifier's tag.
	h.snaps.holding = append(h.snaps.holding, "snap-orphan")
	h.snaps.tagged = map[string]map[string]string{"snap-orphan": {
		rdsSnapshotTagKey:        testSnapshotID,
		rdsSnapshotAccountTagKey: testAccountID,
	}}

	require.NoError(t, rec.reconcileOnce(t.Context()))

	stored, found := h.snapshot(t, testSnapshotID)
	require.True(t, found)
	assert.Equal(t, SnapshotStatusAvailable, stored.Status)
	assert.Equal(t, "snap-orphan", stored.SnapshotID)
	// The worker died before it could report whether the quiesce held.
	assert.True(t, stored.CrashConsistent)
}

// A record left in creating would hold its name forever while naming nothing a
// customer can restore.
func TestReconciler_DoesNotAdoptAnotherAccountsSnapshot(t *testing.T) {
	h := newSnapshotHarness(t, false)
	reconciler := NewReconciler(h.svc, "node-a")

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	stale := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
		CreatedAt:            time.Now().UTC().Add(-2 * snapshotResolveTimeout),
	}
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &stale))
	h.snaps.holding = []string{"snap-other-account", "snap-this-account"}
	h.snaps.tagged = map[string]map[string]string{
		"snap-other-account": {
			rdsSnapshotTagKey:        testSnapshotID,
			rdsSnapshotAccountTagKey: "444455556666",
		},
		"snap-this-account": {
			rdsSnapshotTagKey:        testSnapshotID,
			rdsSnapshotAccountTagKey: testAccountID,
		},
	}

	require.NoError(t, reconciler.reconcileOnce(t.Context()))

	stored, found := h.snapshot(t, testSnapshotID)
	require.True(t, found)
	assert.Equal(t, SnapshotStatusAvailable, stored.Status)
	assert.Equal(t, "snap-this-account", stored.SnapshotID)
}

func TestReconciler_KeepsCreatingSnapshotWhenEC2LookupIsIncomplete(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.snaps.describeErr = errors.New("snapshot metadata temporarily unavailable")
	reconciler := NewReconciler(h.svc, "node-a")

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	stale := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
		CreatedAt:            time.Now().UTC().Add(-2 * snapshotResolveTimeout),
	}
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &stale))

	err = reconciler.reconcileOnce(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata temporarily unavailable")

	stored, found := h.snapshot(t, testSnapshotID)
	require.True(t, found)
	assert.Equal(t, SnapshotStatusCreating, stored.Status)
}

func TestReconciler_DoesNotDeleteAConcurrentlyCompletedSnapshot(t *testing.T) {
	h := newSnapshotHarness(t, false)
	reconciler := NewReconciler(h.svc, "node-a")

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	stale := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
		CreatedAt:            time.Now().UTC().Add(-2 * snapshotResolveTimeout),
	}
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &stale))
	h.snaps.afterDescribe = func() {
		h.snaps.afterDescribe = nil
		var current DBSnapshotRecord
		rev, found, getErr := getJSONRevision(t.Context(), kv, DBSnapshotKey(testSnapshotID), &current)
		require.NoError(t, getErr)
		require.True(t, found)
		current.Status = SnapshotStatusAvailable
		current.SnapshotID = "snap-completed-concurrently"
		require.NoError(t, updateJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), rev, &current))
	}

	require.NoError(t, reconciler.reconcileOnce(t.Context()))

	stored, found := h.snapshot(t, testSnapshotID)
	require.True(t, found)
	assert.Equal(t, SnapshotStatusAvailable, stored.Status)
	assert.Equal(t, "snap-completed-concurrently", stored.SnapshotID)
}

func TestReconciler_RejectsAmbiguousEC2Snapshots(t *testing.T) {
	h := newSnapshotHarness(t, false)
	reconciler := NewReconciler(h.svc, "node-a")

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	stale := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
		CreatedAt:            time.Now().UTC().Add(-2 * snapshotResolveTimeout),
	}
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &stale))
	matchingTags := map[string]string{
		rdsSnapshotTagKey:        testSnapshotID,
		rdsSnapshotAccountTagKey: testAccountID,
	}
	h.snaps.holding = []string{"snap-duplicate-a", "snap-duplicate-b"}
	h.snaps.tagged = map[string]map[string]string{
		"snap-duplicate-a": matchingTags,
		"snap-duplicate-b": matchingTags,
	}

	err = reconciler.reconcileOnce(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple EC2 snapshots")

	stored, found := h.snapshot(t, testSnapshotID)
	require.True(t, found)
	assert.Equal(t, SnapshotStatusCreating, stored.Status)
}

// A completed authoritative lookup is the only evidence that no EC2 snapshot
// was cut. Once it succeeds with no match, the stale name can be released.
func TestReconciler_WithdrawsADBSnapshotWhoseDataWasNeverCut(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := NewReconciler(h.svc, "node-a")

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	stale := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
		CreatedAt:            time.Now().UTC().Add(-2 * snapshotResolveTimeout),
	}
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &stale))

	require.NoError(t, rec.reconcileOnce(t.Context()))

	_, found := h.snapshot(t, testSnapshotID)
	assert.False(t, found)
	assert.Contains(t, eventMessages(h.events(t, EventSourceTypeDBSnapshot, testSnapshotID)),
		"The DB snapshot could not be completed and has been removed.")
}

// Inside the window the record still belongs to a live worker, so touching it
// would race the write that is about to land.
func TestReconciler_LeavesAFreshCreatingSnapshotAlone(t *testing.T) {
	h := newSnapshotHarness(t, false)
	rec := NewReconciler(h.svc, "node-a")

	kv, err := h.svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	fresh := DBSnapshotRecord{
		DBSnapshotIdentifier: testSnapshotID,
		AccountID:            testAccountID,
		Status:               SnapshotStatusCreating,
		CreatedAt:            time.Now().UTC(),
	}
	require.NoError(t, putJSON(t.Context(), kv, DBSnapshotKey(testSnapshotID), &fresh))

	require.NoError(t, rec.reconcileOnce(t.Context()))

	stored, found := h.snapshot(t, testSnapshotID)
	require.True(t, found)
	assert.Equal(t, SnapshotStatusCreating, stored.Status)
}

// A snapshot holds its instance in backing-up for the length of the snapshot and
// no longer, so one still there past the bound belongs to a worker that died.
func TestReconciler_ReturnsAnInstanceStuckInBackingUp(t *testing.T) {
	for _, resume := range []Status{StatusAvailable, StatusStopped} {
		t.Run(string(resume), func(t *testing.T) {
			h := newSnapshotHarness(t, false)
			reconciler := NewReconciler(h.svc, "node-a")

			started := time.Now().UTC().Add(-2 * transitionTimeout)
			rec := availableRecord()
			rec.Status = StatusBackingUp
			rec.TransitionStartedAt = &started
			rec.SnapshotOperation = &SnapshotOperation{
				DBSnapshotIdentifier: testSnapshotID,
				ResumeStatus:         resume,
				StartedAt:            started,
			}
			seedInstance(t, h.svc, rec)

			require.NoError(t, reconciler.reconcileOnce(t.Context()))

			stored := h.instance(t, testDBID)
			assert.Equal(t, resume, stored.Status)
			assert.Nil(t, stored.SnapshotOperation)
		})
	}
}

// Inside the bound the snapshot is still running, and cutting it short would
// leave the engine quiesced with nobody to release it.
func TestReconciler_LeavesAnInFlightSnapshotAlone(t *testing.T) {
	h := newSnapshotHarness(t, false)
	reconciler := NewReconciler(h.svc, "node-a")

	started := time.Now().UTC()
	rec := availableRecord()
	rec.Status = StatusBackingUp
	rec.TransitionStartedAt = &started
	rec.SnapshotOperation = &SnapshotOperation{
		DBSnapshotIdentifier: testSnapshotID,
		ResumeStatus:         StatusAvailable,
		StartedAt:            started,
	}
	seedInstance(t, h.svc, rec)

	require.NoError(t, reconciler.reconcileOnce(t.Context()))

	stored := h.instance(t, testDBID)
	assert.Equal(t, StatusBackingUp, stored.Status)
	assert.NotNil(t, stored.SnapshotOperation)
}

// The Terraform provider reads tags from the describe as well as from
// ListTagsForResource, so the two have to agree.
func TestDBSnapshotTags_ReadBackThroughBothPaths(t *testing.T) {
	h := newSnapshotHarness(t, false)
	h.seedSnapshotSource(t)

	input := snapshotInput()
	input.Tags = []*rds.Tag{{Key: aws.String("env"), Value: aws.String("prod")}}
	out, err := h.svc.CreateDBSnapshot(t.Context(), input, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.DBSnapshot.TagList, 1)

	listed, err := h.svc.ListTagsForResource(t.Context(), &rds.ListTagsForResourceInput{
		ResourceName: aws.String(DBSnapshotARN(testRegion, testAccountID, testSnapshotID)),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.TagList, 1)
	assert.Equal(t, "prod", aws.StringValue(listed.TagList[0].Value))

	_, err = h.svc.AddTagsToResource(t.Context(), &rds.AddTagsToResourceInput{
		ResourceName: aws.String(DBSnapshotARN(testRegion, testAccountID, testSnapshotID)),
		Tags:         []*rds.Tag{{Key: aws.String("team"), Value: aws.String("payments")}},
	}, testAccountID)
	require.NoError(t, err)

	stored, _ := h.snapshot(t, testSnapshotID)
	assert.Equal(t, map[string]string{"env": "prod", "team": "payments"}, stored.Tags)
}

// The record has to survive a round trip through KV, because that is the only
// copy a restore reads once the instance it came from is gone.
func TestDBSnapshotRecord_SurvivesARoundTrip(t *testing.T) {
	updated := time.Now().UTC().Truncate(time.Second)
	rec := DBSnapshotRecord{
		DBSnapshotIdentifier:    testSnapshotID,
		DBInstanceIdentifier:    testDBID,
		AccountID:               testAccountID,
		SnapshotType:            SnapshotTypeManual,
		Status:                  SnapshotStatusAvailable,
		SnapshotID:              "snap-0001",
		SourceVolumeID:          "vol-rdsdata01",
		DBInstanceClass:         "db.t3.medium",
		VpcSecurityGroupIDs:     []string{"sg-app01"},
		DBSubnetGroupName:       "prod-db",
		DBParameterGroupName:    "custom-pg18",
		MasterPasswordUpdatedAt: &updated,
	}
	payload, err := json.Marshal(&rec)
	require.NoError(t, err)

	var back DBSnapshotRecord
	require.NoError(t, json.Unmarshal(payload, &back))
	assert.Equal(t, rec, back)
}
