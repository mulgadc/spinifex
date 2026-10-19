package placementgroup

//test:in-package — the faults are injected by wrapping the service's KV
// handle, which is unexported, and the record layout under test is private.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_AccountTermination_ tests pin how placement groups
// are stored and what Describe and Delete do with them today; a failure means
// it changed. The TestAccountTermination_ tests pin the revision-guarded delete.

// hookedKV fails reads of failGet and runs afterGet once, after read number
// skip+1 of key returns, standing in for a writer on another node.
type hookedKV struct {
	jetstream.KeyValue

	failGet  string
	key      string
	skip     int
	reads    int
	once     sync.Once
	afterGet func()
}

func (k *hookedKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if key == k.failGet {
		return nil, errors.New("injected read failure")
	}
	entry, err := k.KeyValue.Get(ctx, key)
	if key == k.key && k.afterGet != nil {
		if k.reads++; k.reads > k.skip {
			k.once.Do(k.afterGet)
		}
	}
	return entry, err
}

func describedGroupNames(t *testing.T, svc *PlacementGroupServiceImpl, account string) []string {
	t.Helper()
	out, err := svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{}, account)
	require.NoError(t, err)
	var names []string
	for _, g := range out.PlacementGroups {
		names = append(names, aws.StringValue(g.GroupName))
	}
	slices.Sort(names)
	return names
}

func bucketKeys(t *testing.T, kv jetstream.KeyValue) []string {
	t.Helper()
	keys, err := kv.Keys(t.Context())
	require.NoError(t, err)
	slices.Sort(keys)
	return keys
}

// One KV entry per group, keyed <account>.<groupName>, holding the record with
// its tags and member map; the bucket also holds its schema version key.
func TestCurrentBehaviour_AccountTermination_PlacementGroupStorageLayout(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("spread-a"),
		Strategy:  aws.String(ec2.PlacementStrategySpread),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("placement-group"),
			Tags:         []*ec2.Tag{{Key: aws.String("team"), Value: aws.String("blue")}},
		}},
	}, testAccountID)
	require.NoError(t, err)

	assert.Equal(t, []string{testAccountID + ".spread-a", kvutil.VersionKey}, bucketKeys(t, svc.kv))

	entry, err := svc.kv.Get(t.Context(), testAccountID+".spread-a")
	require.NoError(t, err)
	var record PlacementGroupRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &record))
	assert.Equal(t, aws.StringValue(out.PlacementGroup.GroupId), record.GroupId)
	assert.Equal(t, testAccountID, record.AccountID)
	assert.Equal(t, map[string]string{"team": "blue"}, record.Tags)
	assert.Empty(t, record.NodeInstances)
}

// Describe skips a record it cannot read or decode, and still succeeds. Such
// a record cannot be removed through DeletePlacementGroup either, and a read
// failure is an internal error, never an unknown group.
func TestCurrentBehaviour_AccountTermination_DescribePlacementGroupsHidesUnreadableRecords(t *testing.T) {
	brokenKey := kvutil.AccountKey(testAccountID, "broken")
	tests := []struct {
		name       string
		seed       func(t *testing.T, svc *PlacementGroupServiceImpl)
		deleteCode string
	}{
		{"undecodable record", func(t *testing.T, svc *PlacementGroupServiceImpl) {
			_, err := svc.kv.Put(t.Context(), brokenKey, []byte("{not json"))
			require.NoError(t, err)
		}, awserrors.ErrorServerInternal},
		{"record the store cannot read", func(t *testing.T, svc *PlacementGroupServiceImpl) {
			createTestGroup(t, svc, "broken", ec2.PlacementStrategySpread)
			svc.kv = &hookedKV{KeyValue: svc.kv, failGet: brokenKey}
		}, awserrors.ErrorServerInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := setupTestService(t)
			createTestGroup(t, svc, "live", ec2.PlacementStrategySpread)
			tc.seed(t, svc)

			assert.Contains(t, bucketKeys(t, svc.kv), brokenKey)
			assert.Equal(t, []string{"live"}, describedGroupNames(t, svc, testAccountID))

			_, err := svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{GroupName: aws.String("broken")}, testAccountID)
			require.Error(t, err)
			code, _ := awserrors.ResolveErrorCode(err)
			assert.Equal(t, tc.deleteCode, code)
			assert.Contains(t, bucketKeys(t, svc.kv), brokenKey)
		})
	}
}

// The delete commits only at the revision whose membership it checked. A
// launch that records an instance between the check and the delete wins: the
// delete re-reads, refuses with InUse, and the member stays.
func TestAccountTermination_DeletePlacementGroupLosesToAConcurrentMember(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "cluster-a", ec2.PlacementStrategyCluster)
	key := kvutil.AccountKey(testAccountID, "cluster-a")

	launcher := &PlacementGroupServiceImpl{kv: svc.kv, region: svc.region}
	var reserveErr, finalizeErr error
	svc.kv = &hookedKV{KeyValue: svc.kv, key: key, afterGet: func() {
		var reserved *ReserveClusterNodeOutput
		reserved, reserveErr = launcher.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
			GroupName: "cluster-a", EligibleNodes: []string{"node-1"},
		}, testAccountID)
		if reserveErr == nil {
			_, finalizeErr = launcher.FinalizeClusterInstances(context.Background(), &FinalizeClusterInstancesInput{
				GroupName: "cluster-a", NodeInstances: map[string][]string{reserved.TargetNode: {"i-0123456789abcdef0"}},
			}, testAccountID)
		}
	}}

	_, err := svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{GroupName: aws.String("cluster-a")}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupInUse, err.Error())
	require.NoError(t, reserveErr)
	require.NoError(t, finalizeErr, "the interleaved launch committed its member")

	entry, err := launcher.kv.Get(t.Context(), key)
	require.NoError(t, err)
	var record PlacementGroupRecord
	require.NoError(t, json.Unmarshal(entry.Value(), &record))
	assert.Equal(t, map[string][]string{"node-1": {"i-0123456789abcdef0"}}, record.NodeInstances)

	// Once the member leaves, the same delete succeeds.
	_, err = launcher.RemoveInstance(context.Background(), &RemoveInstanceInput{
		GroupName: "cluster-a", NodeName: "node-1", InstanceID: "i-0123456789abcdef0",
	}, testAccountID)
	require.NoError(t, err)
	_, err = launcher.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{GroupName: aws.String("cluster-a")}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, describedGroupNames(t, launcher, testAccountID))
}

// A reservation placeholder is a launch in flight, not a member, so the
// delete may win; the launch's finalize then finds no group and fails, which
// makes the launcher roll its instances back rather than lose them.
func TestAccountTermination_DeletePlacementGroupBeforeFinalizeFailsTheLaunch(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "cluster-a", ec2.PlacementStrategyCluster)
	reserved, err := svc.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
		GroupName: "cluster-a", EligibleNodes: []string{"node-1"},
	}, testAccountID)
	require.NoError(t, err)

	_, err = svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{GroupName: aws.String("cluster-a")}, testAccountID)
	require.NoError(t, err)

	_, err = svc.FinalizeClusterInstances(context.Background(), &FinalizeClusterInstancesInput{
		GroupName: "cluster-a", NodeInstances: map[string][]string{reserved.TargetNode: {"i-0123456789abcdef0"}},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupUnknown, err.Error())
	assert.Empty(t, describedGroupNames(t, svc, testAccountID))
}

// addMember reserves a node in a cluster group and records one instance on it.
func addMember(t *testing.T, svc *PlacementGroupServiceImpl, group, instanceID string) error {
	t.Helper()
	reserved, err := svc.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
		GroupName: group, EligibleNodes: []string{"node-1"},
	}, testAccountID)
	if err != nil {
		return err
	}
	_, err = svc.FinalizeClusterInstances(context.Background(), &FinalizeClusterInstancesInput{
		GroupName: group, NodeInstances: map[string][]string{reserved.TargetNode: {instanceID}},
	}, testAccountID)
	return err
}

// Account cleanup deletes empty groups through the revision-guarded delete,
// reports a group holding instances as blocked with its member count only,
// and leaves records it cannot read for the owner scan to report.
func TestAccountCleanup_DeletesEmptyGroupsAndBlocksOnMembers(t *testing.T) {
	svc := setupTestService(t)
	empty := createTestGroup(t, svc, "empty", ec2.PlacementStrategySpread)
	held := createTestGroup(t, svc, "held", ec2.PlacementStrategyCluster)
	require.NoError(t, addMember(t, svc, "held", "i-0123456789abcdef0"))
	_, err := svc.kv.Put(t.Context(), kvutil.AccountKey(testAccountID, "broken"), []byte("{not json"))
	require.NoError(t, err)
	other := "999988887777"
	_, err = svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("theirs"), Strategy: aws.String(ec2.PlacementStrategySpread),
	}, other)
	require.NoError(t, err)

	out, err := svc.CleanupAccount(t.Context(), testAccountID, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{aws.StringValue(empty.GroupId)}, out.DeletedGroupIDs)
	assert.Equal(t, []BlockedGroup{{GroupID: aws.StringValue(held.GroupId), MemberCount: 1}}, out.Blocked)
	assert.Empty(t, out.FailedGroupIDs)

	assert.Equal(t, []string{"held"}, describedGroupNames(t, svc, testAccountID))
	assert.Equal(t, []string{"theirs"}, describedGroupNames(t, svc, other))
	_, err = svc.kv.Get(t.Context(), kvutil.AccountKey(testAccountID, "broken"))
	require.NoError(t, err, "an unreadable record is never deleted")
}

// A launch that records a member between the cleanup's membership check and
// its delete wins: the cleanup reports the group blocked and the member stays.
func TestAccountCleanup_ConcurrentMemberBlocksTheDelete(t *testing.T) {
	svc := setupTestService(t)
	group := createTestGroup(t, svc, "cluster-a", ec2.PlacementStrategyCluster)
	key := kvutil.AccountKey(testAccountID, "cluster-a")
	launcher := &PlacementGroupServiceImpl{kv: svc.kv, region: svc.region}
	var memberErr error
	// Read 1 is the cleanup's listing read; read 2 is the delete's check.
	svc.kv = &hookedKV{KeyValue: svc.kv, key: key, skip: 1, afterGet: func() {
		memberErr = addMember(t, launcher, "cluster-a", "i-0123456789abcdef0")
	}}

	out, err := svc.CleanupAccount(t.Context(), testAccountID, nil)
	require.NoError(t, err)
	require.NoError(t, memberErr, "the interleaved launch committed its member")
	assert.Equal(t, []BlockedGroup{{GroupID: aws.StringValue(group.GroupId), MemberCount: 1}}, out.Blocked)
	assert.Empty(t, out.DeletedGroupIDs)
	assert.Equal(t, map[string][]string{"node-1": {"i-0123456789abcdef0"}}, readRecord(t, launcher, "cluster-a").NodeInstances)
}

// A group the caller's reference check names is kept with the given reason,
// whether it is matched by its name or its group ID.
func TestAccountCleanup_KeepsReferencedGroups(t *testing.T) {
	svc := setupTestService(t)
	named := createTestGroup(t, svc, "named", ec2.PlacementStrategySpread)
	byID := createTestGroup(t, svc, "by-id", ec2.PlacementStrategySpread)
	free := createTestGroup(t, svc, "free", ec2.PlacementStrategySpread)
	referenced := func(id, name string) string {
		switch {
		case name == "named":
			return "spot-request-reference"
		case id == aws.StringValue(byID.GroupId):
			return "launch-template-reference"
		}
		return ""
	}

	out, err := svc.CleanupAccount(t.Context(), testAccountID, referenced)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		aws.StringValue(named.GroupId): "spot-request-reference",
		aws.StringValue(byID.GroupId):  "launch-template-reference",
	}, out.Referenced)
	assert.Equal(t, []string{aws.StringValue(free.GroupId)}, out.DeletedGroupIDs)
	assert.ElementsMatch(t, []string{"named", "by-id"}, describedGroupNames(t, svc, testAccountID))
}

// A listing failure is returned and nothing is deleted.
func TestAccountCleanup_ListingFailureDeletesNothing(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "empty", ec2.PlacementStrategySpread)
	kv := svc.kv
	svc.kv = &listFailingKV{KeyValue: kv}

	_, err := svc.CleanupAccount(t.Context(), testAccountID, nil)
	require.Error(t, err)
	svc.kv = kv
	assert.Equal(t, []string{"empty"}, describedGroupNames(t, svc, testAccountID))
}

// listFailingKV fails every key listing.
type listFailingKV struct {
	jetstream.KeyValue
}

func (k *listFailingKV) ListKeys(context.Context, ...jetstream.WatchOpt) (jetstream.KeyLister, error) {
	return nil, errors.New("injected listing failure")
}

func (k *listFailingKV) Keys(context.Context, ...jetstream.WatchOpt) ([]string, error) {
	return nil, errors.New("injected listing failure")
}
