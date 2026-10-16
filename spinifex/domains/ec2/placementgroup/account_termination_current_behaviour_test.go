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
// are stored and what Describe and Delete do with them today. They describe
// current behaviour, not the target design; a failure means it changed.

// hookedKV fails reads of failGet and runs afterGet once, after the first read
// of key returns, standing in for a writer on another node.
type hookedKV struct {
	jetstream.KeyValue

	failGet  string
	key      string
	once     sync.Once
	afterGet func()
}

func (k *hookedKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	if key == k.failGet {
		return nil, errors.New("injected read failure")
	}
	entry, err := k.KeyValue.Get(ctx, key)
	if key == k.key && k.afterGet != nil {
		k.once.Do(k.afterGet)
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

// Describe skips a record it cannot read or decode, and still succeeds. An
// undecodable record also cannot be removed through DeletePlacementGroup.
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
		}, awserrors.ErrorInvalidPlacementGroupUnknown},
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

// Pins current behaviour that differs from AWS: the delete reads the record,
// checks membership, then deletes without a revision guard. A launch that
// records an instance between the two is deleted with the group.
func TestCurrentBehaviour_AccountTermination_DeletePlacementGroupDropsAConcurrentMember(t *testing.T) {
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
	require.NoError(t, err, "the delete succeeds although the group gained a member")
	require.NoError(t, reserveErr)
	require.NoError(t, finalizeErr, "the interleaved launch committed its member")

	_, err = launcher.kv.Get(t.Context(), key)
	assert.ErrorIs(t, err, jetstream.ErrKeyNotFound)
	assert.Empty(t, describedGroupNames(t, launcher, testAccountID))

	// The member's later shutdown finds no group and reports success.
	_, err = launcher.RemoveInstance(context.Background(), &RemoveInstanceInput{
		GroupName: "cluster-a", NodeName: "node-1", InstanceID: "i-0123456789abcdef0",
	}, testAccountID)
	assert.NoError(t, err)
}
