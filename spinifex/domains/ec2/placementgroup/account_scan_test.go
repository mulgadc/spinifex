package placementgroup

//test:in-package — the scan is run over a read-only wrapper of the service's
// unexported KV handle, and records are seeded in their private layout.

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/internal/testkit/readonlystore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvutil"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keysFailingKV fails every key listing, standing in for a KV outage.
type keysFailingKV struct {
	jetstream.KeyValue
}

func (k *keysFailingKV) Keys(context.Context, ...jetstream.WatchOpt) ([]string, error) {
	return nil, errors.New("injected listing failure")
}

// Groups are reported with the instances their member map names; a node
// placeholder left by a reservation is not a member. Undecodable and unreadable
// records are counted, the second also as a read failure.
func TestScanAccountClassifiesPlacementGroupRecords(t *testing.T) {
	svc := setupTestService(t)
	empty := createTestGroup(t, svc, "empty", ec2.PlacementStrategySpread)
	reserved := createTestGroup(t, svc, "reserved", ec2.PlacementStrategySpread)
	_, err := svc.ReserveSpreadNodes(context.Background(), &ReserveSpreadNodesInput{
		GroupName: "reserved", EligibleNodes: []string{"node-1"}, MinCount: 1, MaxCount: 1,
	}, testAccountID)
	require.NoError(t, err)
	held := createTestGroup(t, svc, "held", ec2.PlacementStrategyCluster)
	_, err = svc.FinalizeClusterInstances(context.Background(), &FinalizeClusterInstancesInput{
		GroupName: "held", NodeInstances: map[string][]string{"node-2": {"i-0bbb", "i-0aaa"}, "node-3": {"i-0aaa"}},
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.kv.Put(t.Context(), kvutil.AccountKey(testAccountID, "undecodable"), []byte("{not json"))
	require.NoError(t, err)
	_, err = svc.kv.Put(t.Context(), kvutil.AccountKey(testAccountID, "anonymous"), []byte(`{"group_name":"anonymous"}`))
	require.NoError(t, err)
	createTestGroup(t, svc, "unreadable", ec2.PlacementStrategySpread)
	require.Contains(t, bucketKeys(t, svc.kv), kvutil.VersionKey)

	svc.kv = &hookedKV{KeyValue: readonlystore.NewKV(t, svc.kv), failGet: kvutil.AccountKey(testAccountID, "unreadable")}
	scan, err := svc.ScanAccount(context.Background(), testAccountID)
	require.NoError(t, err)

	assert.ElementsMatch(t, []ScannedGroup{
		{GroupID: *empty.GroupId},
		{GroupID: *reserved.GroupId},
		{GroupID: *held.GroupId, MemberInstanceIDs: []string{"i-0aaa", "i-0bbb"}},
	}, scan.Groups)
	assert.Equal(t, 3, scan.Unreadable)
	assert.Equal(t, 1, scan.ReadFailures)
}

// The account prefix ends at its dot, so account 1111 never sees 11112's groups.
func TestScanAccountStaysInsideThePlacementGroupAccountPrefix(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{GroupName: aws.String("neighbour")}, "11112")
	require.NoError(t, err)
	svc.kv = readonlystore.NewKV(t, svc.kv)

	scan, err := svc.ScanAccount(context.Background(), "1111")
	require.NoError(t, err)
	assert.Equal(t, &AccountScan{}, scan)

	scan, err = svc.ScanAccount(context.Background(), "11112")
	require.NoError(t, err)
	assert.Len(t, scan.Groups, 1)
}

// A listing failure is an error, never an empty scan.
func TestScanAccountReturnsAPlacementGroupListingFailure(t *testing.T) {
	svc := setupTestService(t)
	svc.kv = &keysFailingKV{KeyValue: svc.kv}
	_, err := svc.ScanAccount(context.Background(), testAccountID)
	require.Error(t, err)
}
