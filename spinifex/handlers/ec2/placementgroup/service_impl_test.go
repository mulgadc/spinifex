package handlers_ec2_placementgroup

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "123456789012"

func setupTestService(t *testing.T) *PlacementGroupServiceImpl {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)

	svc, err := NewPlacementGroupServiceImplWithNATS(nil, nc)
	require.NoError(t, err)
	return svc
}

func createTestGroup(t *testing.T, svc *PlacementGroupServiceImpl, name, strategy string) *ec2.PlacementGroup {
	t.Helper()
	out, err := svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String(name),
		Strategy:  aws.String(strategy),
	}, testAccountID)
	require.NoError(t, err)
	return out.PlacementGroup
}

// --- CreatePlacementGroup Tests ---

func TestCreatePlacementGroup_Spread(t *testing.T) {
	svc := setupTestService(t)
	pg := createTestGroup(t, svc, "my-spread-group", "spread")

	assert.Equal(t, "pg-", (*pg.GroupId)[:3])
	assert.Equal(t, "my-spread-group", *pg.GroupName)
	assert.Equal(t, "spread", *pg.Strategy)
	assert.Equal(t, "available", *pg.State)
	assert.Equal(t, "host", *pg.SpreadLevel)
}

func TestCreatePlacementGroup_Cluster(t *testing.T) {
	svc := setupTestService(t)
	pg := createTestGroup(t, svc, "my-cluster-group", "cluster")

	assert.Equal(t, "pg-", (*pg.GroupId)[:3])
	assert.Equal(t, "my-cluster-group", *pg.GroupName)
	assert.Equal(t, "cluster", *pg.Strategy)
	assert.Equal(t, "available", *pg.State)
	// SpreadLevel should be nil for cluster strategy
	assert.Nil(t, pg.SpreadLevel)
}

func TestCreatePlacementGroup_DuplicateName(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "dup-group", "spread")

	_, err := svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("dup-group"),
		Strategy:  aws.String("spread"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupDuplicate, err.Error())
}

func TestCreatePlacementGroup_PartitionRejected(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("part-group"),
		Strategy:  aws.String("partition"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestCreatePlacementGroup_InvalidStrategy(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("bad-group"),
		Strategy:  aws.String("invalid"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestCreatePlacementGroup_MissingName(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		Strategy: aws.String("spread"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestCreatePlacementGroup_MissingStrategyDefaultsToCluster(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("no-strat"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "cluster", *out.PlacementGroup.Strategy)
}

func TestCreatePlacementGroup_EmptyStrategyDefaultsToCluster(t *testing.T) {
	svc := setupTestService(t)
	out, err := svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("empty-strat"),
		Strategy:  aws.String(""),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "cluster", *out.PlacementGroup.Strategy)
}

func TestCreatePlacementGroup_SameNameDifferentAccounts(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "shared-name", "spread")

	// Different account should succeed
	out, err := svc.CreatePlacementGroup(context.Background(), &ec2.CreatePlacementGroupInput{
		GroupName: aws.String("shared-name"),
		Strategy:  aws.String("cluster"),
	}, "999999999999")
	require.NoError(t, err)
	assert.Equal(t, "shared-name", *out.PlacementGroup.GroupName)
}

// --- DeletePlacementGroup Tests ---

func TestDeletePlacementGroup_Success(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "del-group", "spread")

	_, err := svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{
		GroupName: aws.String("del-group"),
	}, testAccountID)
	require.NoError(t, err)

	// Verify it's gone
	out, err := svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.PlacementGroups)
}

func TestDeletePlacementGroup_NotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{
		GroupName: aws.String("nonexistent"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupUnknown, err.Error())
}

func TestDeletePlacementGroup_InUse(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "in-use-group", "spread")

	// Simulate instances by updating the record directly
	record, entry, err := svc.GetPlacementGroupRecord(testAccountID, "in-use-group")
	require.NoError(t, err)
	record.NodeInstances["node1"] = []string{"i-123"}
	err = svc.UpdatePlacementGroupRecord(testAccountID, "in-use-group", record, entry.Revision())
	require.NoError(t, err)

	_, err = svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{
		GroupName: aws.String("in-use-group"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupInUse, err.Error())
}

func TestDeletePlacementGroup_MissingName(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

// --- DescribePlacementGroups Tests ---

func TestDescribePlacementGroups_All(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "group-a", "spread")
	createTestGroup(t, svc, "group-b", "cluster")

	out, err := svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.PlacementGroups, 2)
}

func TestDescribePlacementGroups_FilterByName(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "alpha", "spread")
	createTestGroup(t, svc, "beta", "cluster")

	out, err := svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{
		GroupNames: []*string{aws.String("alpha")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.PlacementGroups, 1)
	assert.Equal(t, "alpha", *out.PlacementGroups[0].GroupName)
}

func TestDescribePlacementGroups_FilterByGroupId(t *testing.T) {
	svc := setupTestService(t)
	pg := createTestGroup(t, svc, "id-filter", "spread")

	out, err := svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{
		GroupIds: []*string{pg.GroupId},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.PlacementGroups, 1)
	assert.Equal(t, *pg.GroupId, *out.PlacementGroups[0].GroupId)
}

func TestDescribePlacementGroups_FilterByGroupIdFilter(t *testing.T) {
	svc := setupTestService(t)
	pg := createTestGroup(t, svc, "gid-filter", "spread")
	createTestGroup(t, svc, "other-group", "cluster")

	// Exact match
	out, err := svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-id"), Values: []*string{pg.GroupId}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.PlacementGroups, 1)
	assert.Equal(t, *pg.GroupId, *out.PlacementGroups[0].GroupId)

	// Non-match
	out, err = svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-id"), Values: []*string{aws.String("pg-000000")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.PlacementGroups)

	// Wildcard
	out, err = svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("group-id"), Values: []*string{aws.String("pg-*")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.PlacementGroups, 2)
}

func TestDescribePlacementGroups_FilterByStrategy(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "spread1", "spread")
	createTestGroup(t, svc, "cluster1", "cluster")

	out, err := svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("strategy"), Values: []*string{aws.String("spread")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.PlacementGroups, 1)
	assert.Equal(t, "spread1", *out.PlacementGroups[0].GroupName)
}

func TestDescribePlacementGroups_FilterByState(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "avail-group", "spread")

	out, err := svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("state"), Values: []*string{aws.String("available")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.PlacementGroups, 1)

	out, err = svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("state"), Values: []*string{aws.String("deleting")}},
		},
	}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.PlacementGroups)
}

func TestDescribePlacementGroups_NameNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{
		GroupNames: []*string{aws.String("ghost")},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupUnknown, err.Error())
}

func TestDescribePlacementGroups_AccountScoped(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "acct-group", "spread")

	// Different account should see nothing
	out, err := svc.DescribePlacementGroups(context.Background(), &ec2.DescribePlacementGroupsInput{}, "999999999999")
	require.NoError(t, err)
	assert.Empty(t, out.PlacementGroups)
}

// --- NodeInstances CAS Tests ---

func TestGetAndUpdatePlacementGroupRecord(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "cas-group", "spread")

	// Get with revision
	record, entry, err := svc.GetPlacementGroupRecord(testAccountID, "cas-group")
	require.NoError(t, err)
	assert.Equal(t, "spread", record.Strategy)
	assert.Empty(t, record.NodeInstances)

	// Update with CAS
	record.NodeInstances["node1"] = []string{"i-abc"}
	err = svc.UpdatePlacementGroupRecord(testAccountID, "cas-group", record, entry.Revision())
	require.NoError(t, err)

	// Verify update
	record2, _, err := svc.GetPlacementGroupRecord(testAccountID, "cas-group")
	require.NoError(t, err)
	assert.Equal(t, []string{"i-abc"}, record2.NodeInstances["node1"])
}

func TestUpdatePlacementGroupRecord_CASConflict(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "conflict-group", "spread")

	// Get the record twice (same revision)
	record1, entry1, err := svc.GetPlacementGroupRecord(testAccountID, "conflict-group")
	require.NoError(t, err)
	record2, _, err := svc.GetPlacementGroupRecord(testAccountID, "conflict-group")
	require.NoError(t, err)

	// First update succeeds
	record1.NodeInstances["node1"] = []string{"i-111"}
	err = svc.UpdatePlacementGroupRecord(testAccountID, "conflict-group", record1, entry1.Revision())
	require.NoError(t, err)

	// Second update with stale revision fails
	record2.NodeInstances["node2"] = []string{"i-222"}
	err = svc.UpdatePlacementGroupRecord(testAccountID, "conflict-group", record2, entry1.Revision())
	require.Error(t, err)
}

func TestGetPlacementGroupRecord_NotFound(t *testing.T) {
	svc := setupTestService(t)
	_, _, err := svc.GetPlacementGroupRecord(testAccountID, "nonexistent")
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupUnknown, err.Error())
}

// --- ReserveSpreadNodes Tests ---

func TestReserveSpreadNodes_Success(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "reserve-group", "spread")

	out, err := svc.ReserveSpreadNodes(context.Background(), &ReserveSpreadNodesInput{
		GroupName:     "reserve-group",
		EligibleNodes: []string{"node-a", "node-b", "node-c"},
		MinCount:      2,
		MaxCount:      3,
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.ReservedNodes, 3)

	// Verify placeholders in record
	record, _, err := svc.GetPlacementGroupRecord(testAccountID, "reserve-group")
	require.NoError(t, err)
	assert.Len(t, record.NodeInstances, 3)
	for _, node := range out.ReservedNodes {
		_, ok := record.NodeInstances[node]
		assert.True(t, ok, "node %s should be in NodeInstances", node)
	}
}

func TestReserveSpreadNodes_ExcludesOccupiedNodes(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "occupied-group", "spread")

	// Pre-occupy node-a
	record, entry, err := svc.GetPlacementGroupRecord(testAccountID, "occupied-group")
	require.NoError(t, err)
	record.NodeInstances["node-a"] = []string{"i-existing"}
	require.NoError(t, svc.UpdatePlacementGroupRecord(testAccountID, "occupied-group", record, entry.Revision()))

	out, err := svc.ReserveSpreadNodes(context.Background(), &ReserveSpreadNodesInput{
		GroupName:     "occupied-group",
		EligibleNodes: []string{"node-a", "node-b", "node-c"},
		MinCount:      1,
		MaxCount:      2,
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, out.ReservedNodes, 2)
	// node-a should NOT be in reserved nodes
	for _, n := range out.ReservedNodes {
		assert.NotEqual(t, "node-a", n)
	}
}

func TestReserveSpreadNodes_InsufficientNodes(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "insufficient-group", "spread")

	// Pre-occupy node-a and node-b
	record, entry, err := svc.GetPlacementGroupRecord(testAccountID, "insufficient-group")
	require.NoError(t, err)
	record.NodeInstances["node-a"] = []string{"i-1"}
	record.NodeInstances["node-b"] = []string{"i-2"}
	require.NoError(t, svc.UpdatePlacementGroupRecord(testAccountID, "insufficient-group", record, entry.Revision()))

	_, err = svc.ReserveSpreadNodes(context.Background(), &ReserveSpreadNodesInput{
		GroupName:     "insufficient-group",
		EligibleNodes: []string{"node-a", "node-b", "node-c"},
		MinCount:      2,
		MaxCount:      2,
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

func TestReserveSpreadNodes_WrongStrategy(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "cluster-group", "cluster")

	_, err := svc.ReserveSpreadNodes(context.Background(), &ReserveSpreadNodesInput{
		GroupName:     "cluster-group",
		EligibleNodes: []string{"node-a"},
		MinCount:      1,
		MaxCount:      1,
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestReserveSpreadNodes_GroupNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.ReserveSpreadNodes(context.Background(), &ReserveSpreadNodesInput{
		GroupName:     "ghost-group",
		EligibleNodes: []string{"node-a"},
		MinCount:      1,
		MaxCount:      1,
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupUnknown, err.Error())
}

// --- FinalizeSpreadInstances Tests ---

func TestFinalizeSpreadInstances_Success(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "finalize-group", "spread")

	// Reserve nodes first
	_, err := svc.ReserveSpreadNodes(context.Background(), &ReserveSpreadNodesInput{
		GroupName:     "finalize-group",
		EligibleNodes: []string{"node-a", "node-b"},
		MinCount:      2,
		MaxCount:      2,
	}, testAccountID)
	require.NoError(t, err)

	// Finalize with instance IDs
	_, err = svc.FinalizeSpreadInstances(context.Background(), &FinalizeSpreadInstancesInput{
		GroupName: "finalize-group",
		NodeInstances: map[string][]string{
			"node-a": {"i-aaa"},
			"node-b": {"i-bbb"},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify
	record, _, err := svc.GetPlacementGroupRecord(testAccountID, "finalize-group")
	require.NoError(t, err)
	assert.Equal(t, []string{"i-aaa"}, record.NodeInstances["node-a"])
	assert.Equal(t, []string{"i-bbb"}, record.NodeInstances["node-b"])
}

// --- ReleaseSpreadNodes Tests ---

func TestReleaseSpreadNodes_Success(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "release-group", "spread")

	// Reserve nodes
	_, err := svc.ReserveSpreadNodes(context.Background(), &ReserveSpreadNodesInput{
		GroupName:     "release-group",
		EligibleNodes: []string{"node-a", "node-b"},
		MinCount:      2,
		MaxCount:      2,
	}, testAccountID)
	require.NoError(t, err)

	// Release node-b
	_, err = svc.ReleaseSpreadNodes(context.Background(), &ReleaseSpreadNodesInput{
		GroupName: "release-group",
		Nodes:     []string{"node-b"},
	}, testAccountID)
	require.NoError(t, err)

	// Verify only node-a remains
	record, _, err := svc.GetPlacementGroupRecord(testAccountID, "release-group")
	require.NoError(t, err)
	assert.Len(t, record.NodeInstances, 1)
	_, ok := record.NodeInstances["node-a"]
	assert.True(t, ok)
}

func TestReleaseSpreadNodes_AllNodes(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "release-all-group", "spread")

	// Reserve nodes
	_, err := svc.ReserveSpreadNodes(context.Background(), &ReserveSpreadNodesInput{
		GroupName:     "release-all-group",
		EligibleNodes: []string{"node-a", "node-b"},
		MinCount:      2,
		MaxCount:      2,
	}, testAccountID)
	require.NoError(t, err)

	// Release all
	_, err = svc.ReleaseSpreadNodes(context.Background(), &ReleaseSpreadNodesInput{
		GroupName: "release-all-group",
		Nodes:     []string{"node-a", "node-b"},
	}, testAccountID)
	require.NoError(t, err)

	// Verify empty
	record, _, err := svc.GetPlacementGroupRecord(testAccountID, "release-all-group")
	require.NoError(t, err)
	assert.Empty(t, record.NodeInstances)
}

// --- RemoveInstance Tests ---

func TestRemoveInstance_Success(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "remove-inst-group", "spread")

	// Add instances to two nodes
	record, entry, err := svc.GetPlacementGroupRecord(testAccountID, "remove-inst-group")
	require.NoError(t, err)
	record.NodeInstances["node-a"] = []string{"i-aaa"}
	record.NodeInstances["node-b"] = []string{"i-bbb"}
	require.NoError(t, svc.UpdatePlacementGroupRecord(testAccountID, "remove-inst-group", record, entry.Revision()))

	// Remove i-aaa from node-a
	_, err = svc.RemoveInstance(context.Background(), &RemoveInstanceInput{
		GroupName:  "remove-inst-group",
		NodeName:   "node-a",
		InstanceID: "i-aaa",
	}, testAccountID)
	require.NoError(t, err)

	// Verify node-a is removed (was the only instance), node-b remains
	record, _, err = svc.GetPlacementGroupRecord(testAccountID, "remove-inst-group")
	require.NoError(t, err)
	assert.Len(t, record.NodeInstances, 1)
	_, hasA := record.NodeInstances["node-a"]
	assert.False(t, hasA)
	assert.Equal(t, []string{"i-bbb"}, record.NodeInstances["node-b"])
}

// TestRemoveInstance_NodeNotFound / _GroupNotFound pin the idempotency contract:
// RemoveInstance must succeed when the node or group is missing (callers may race
// with group deletion or have already moved the instance off the node).

func TestRemoveInstance_NodeNotFound(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "remove-nonode-group", "spread")

	_, err := svc.RemoveInstance(context.Background(), &RemoveInstanceInput{
		GroupName:  "remove-nonode-group",
		NodeName:   "ghost-node",
		InstanceID: "i-xxx",
	}, testAccountID)
	require.NoError(t, err)
}

func TestRemoveInstance_GroupNotFound(t *testing.T) {
	svc := setupTestService(t)

	_, err := svc.RemoveInstance(context.Background(), &RemoveInstanceInput{
		GroupName:  "deleted-group",
		NodeName:   "node-a",
		InstanceID: "i-xxx",
	}, testAccountID)
	require.NoError(t, err)
}

func TestRemoveInstance_MultipleInstancesOnNode(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "multi-inst-group", "cluster")

	// Add multiple instances to same node (cluster scenario)
	record, entry, err := svc.GetPlacementGroupRecord(testAccountID, "multi-inst-group")
	require.NoError(t, err)
	record.NodeInstances["node-a"] = []string{"i-111", "i-222", "i-333"}
	require.NoError(t, svc.UpdatePlacementGroupRecord(testAccountID, "multi-inst-group", record, entry.Revision()))

	// Remove i-222
	_, err = svc.RemoveInstance(context.Background(), &RemoveInstanceInput{
		GroupName:  "multi-inst-group",
		NodeName:   "node-a",
		InstanceID: "i-222",
	}, testAccountID)
	require.NoError(t, err)

	// Verify i-111 and i-333 remain
	record, _, err = svc.GetPlacementGroupRecord(testAccountID, "multi-inst-group")
	require.NoError(t, err)
	assert.Equal(t, []string{"i-111", "i-333"}, record.NodeInstances["node-a"])
}

// --- ReserveClusterNode Tests ---

func TestReserveClusterNode_EmptyGroup(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "cluster-empty", "cluster")

	out, err := svc.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
		GroupName:     "cluster-empty",
		EligibleNodes: []string{"node-a", "node-b", "node-c"},
	}, testAccountID)
	require.NoError(t, err)
	// Should pick first eligible node (highest capacity, sorted by caller)
	assert.Equal(t, "node-a", out.TargetNode)

	// Verify placeholder written
	record, _, err := svc.GetPlacementGroupRecord(testAccountID, "cluster-empty")
	require.NoError(t, err)
	assert.Len(t, record.NodeInstances, 1)
	_, ok := record.NodeInstances["node-a"]
	assert.True(t, ok)
}

func TestReserveClusterNode_ExistingNode(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "cluster-existing", "cluster")

	// Pre-populate with instances on node-b
	record, entry, err := svc.GetPlacementGroupRecord(testAccountID, "cluster-existing")
	require.NoError(t, err)
	record.NodeInstances["node-b"] = []string{"i-existing"}
	require.NoError(t, svc.UpdatePlacementGroupRecord(testAccountID, "cluster-existing", record, entry.Revision()))

	// Even though node-a is in eligible list, should return existing node-b
	out, err := svc.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
		GroupName:     "cluster-existing",
		EligibleNodes: []string{"node-a", "node-c"},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "node-b", out.TargetNode)
}

func TestReserveClusterNode_NoEligibleNodes(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "cluster-noelig", "cluster")

	_, err := svc.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
		GroupName:     "cluster-noelig",
		EligibleNodes: []string{},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInsufficientInstanceCapacity, err.Error())
}

func TestReserveClusterNode_WrongStrategy(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "spread-for-cluster", "spread")

	_, err := svc.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
		GroupName:     "spread-for-cluster",
		EligibleNodes: []string{"node-a"},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestReserveClusterNode_GroupNotFound(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
		GroupName:     "ghost-cluster",
		EligibleNodes: []string{"node-a"},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupUnknown, err.Error())
}

func TestReserveClusterNode_MissingGroupName(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
		EligibleNodes: []string{"node-a"},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

// --- FinalizeClusterInstances Tests ---

func TestFinalizeClusterInstances_Success(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "cluster-finalize", "cluster")

	// Reserve a node first
	_, err := svc.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
		GroupName:     "cluster-finalize",
		EligibleNodes: []string{"node-a"},
	}, testAccountID)
	require.NoError(t, err)

	// Finalize with instance IDs
	_, err = svc.FinalizeClusterInstances(context.Background(), &FinalizeClusterInstancesInput{
		GroupName: "cluster-finalize",
		NodeInstances: map[string][]string{
			"node-a": {"i-c1", "i-c2", "i-c3"},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify instances recorded
	record, _, err := svc.GetPlacementGroupRecord(testAccountID, "cluster-finalize")
	require.NoError(t, err)
	assert.Equal(t, []string{"i-c1", "i-c2", "i-c3"}, record.NodeInstances["node-a"])
}

func TestFinalizeClusterInstances_AppendsToExisting(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "cluster-append", "cluster")

	// Pre-populate with existing instances
	record, entry, err := svc.GetPlacementGroupRecord(testAccountID, "cluster-append")
	require.NoError(t, err)
	record.NodeInstances["node-a"] = []string{"i-existing1", "i-existing2"}
	require.NoError(t, svc.UpdatePlacementGroupRecord(testAccountID, "cluster-append", record, entry.Revision()))

	// Finalize with new instances (should append, not overwrite)
	_, err = svc.FinalizeClusterInstances(context.Background(), &FinalizeClusterInstancesInput{
		GroupName: "cluster-append",
		NodeInstances: map[string][]string{
			"node-a": {"i-new1", "i-new2"},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Verify all instances present
	record, _, err = svc.GetPlacementGroupRecord(testAccountID, "cluster-append")
	require.NoError(t, err)
	assert.Equal(t, []string{"i-existing1", "i-existing2", "i-new1", "i-new2"}, record.NodeInstances["node-a"])
}

func TestFinalizeClusterInstances_MissingGroupName(t *testing.T) {
	svc := setupTestService(t)
	_, err := svc.FinalizeClusterInstances(context.Background(), &FinalizeClusterInstancesInput{
		NodeInstances: map[string][]string{"node-a": {"i-1"}},
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

// --- Cluster Lifecycle Test ---

func TestClusterLifecycle_ReserveFinalizeRemoveDelete(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "cluster-lifecycle", "cluster")

	// Reserve node
	reserveOut, err := svc.ReserveClusterNode(context.Background(), &ReserveClusterNodeInput{
		GroupName:     "cluster-lifecycle",
		EligibleNodes: []string{"node-1"},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "node-1", reserveOut.TargetNode)

	// Finalize with instances
	_, err = svc.FinalizeClusterInstances(context.Background(), &FinalizeClusterInstancesInput{
		GroupName: "cluster-lifecycle",
		NodeInstances: map[string][]string{
			"node-1": {"i-c1", "i-c2"},
		},
	}, testAccountID)
	require.NoError(t, err)

	// Can't delete — instances present
	_, err = svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{
		GroupName: aws.String("cluster-lifecycle"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupInUse, err.Error())

	// Remove instances via RemoveInstance (simulating terminate)
	_, err = svc.RemoveInstance(context.Background(), &RemoveInstanceInput{
		GroupName: "cluster-lifecycle", NodeName: "node-1", InstanceID: "i-c1",
	}, testAccountID)
	require.NoError(t, err)
	_, err = svc.RemoveInstance(context.Background(), &RemoveInstanceInput{
		GroupName: "cluster-lifecycle", NodeName: "node-1", InstanceID: "i-c2",
	}, testAccountID)
	require.NoError(t, err)

	// Now delete succeeds
	_, err = svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{
		GroupName: aws.String("cluster-lifecycle"),
	}, testAccountID)
	require.NoError(t, err)
}

// --- End-to-End Spread Lifecycle Test ---

func TestSpreadLifecycle_ReserveFinalizeDelete(t *testing.T) {
	svc := setupTestService(t)
	createTestGroup(t, svc, "lifecycle-group", "spread")

	// Reserve 2 nodes
	reserveOut, err := svc.ReserveSpreadNodes(context.Background(), &ReserveSpreadNodesInput{
		GroupName:     "lifecycle-group",
		EligibleNodes: []string{"node-1", "node-2", "node-3"},
		MinCount:      2,
		MaxCount:      2,
	}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, reserveOut.ReservedNodes, 2)

	// Finalize with instance IDs
	nodeInstances := make(map[string][]string)
	for _, n := range reserveOut.ReservedNodes {
		nodeInstances[n] = []string{"i-" + n}
	}
	_, err = svc.FinalizeSpreadInstances(context.Background(), &FinalizeSpreadInstancesInput{
		GroupName:     "lifecycle-group",
		NodeInstances: nodeInstances,
	}, testAccountID)
	require.NoError(t, err)

	// Can't delete — instances present
	_, err = svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{
		GroupName: aws.String("lifecycle-group"),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidPlacementGroupInUse, err.Error())

	// Remove instances via RemoveInstance (simulating terminate)
	for _, n := range reserveOut.ReservedNodes {
		_, err = svc.RemoveInstance(context.Background(), &RemoveInstanceInput{
			GroupName:  "lifecycle-group",
			NodeName:   n,
			InstanceID: "i-" + n,
		}, testAccountID)
		require.NoError(t, err)
	}

	// Now delete succeeds
	_, err = svc.DeletePlacementGroup(context.Background(), &ec2.DeletePlacementGroupInput{
		GroupName: aws.String("lifecycle-group"),
	}, testAccountID)
	require.NoError(t, err)
}
