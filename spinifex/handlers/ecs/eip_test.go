package handlers_ecs

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEIPs records auto-EIP calls so the RUNNING/STOPPED hooks can be asserted
// without a live ec2 EIP daemon.
type stubEIPs struct {
	allocated  []string // eniIDs associated
	released   []string // allocation IDs released
	publicIP   string
	allocID    string
	allocErr   error
	releaseErr error
}

func (e *stubEIPs) AllocateAndAssociate(_ context.Context, _, eniID string) (string, string, error) {
	if e.allocErr != nil {
		return "", "", e.allocErr
	}
	e.allocated = append(e.allocated, eniID)
	return e.publicIP, e.allocID, nil
}

func (e *stubEIPs) Release(_ context.Context, _, allocationID string) error {
	e.released = append(e.released, allocationID)
	return e.releaseErr
}

func TestAssignTaskPublicIP_EnabledAssociatesAndPersists(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	eips := &stubEIPs{publicIP: "192.0.2.50", allocID: "eipalloc-1"}
	svc.eips = eips

	require.NoError(t, putJSON(kv, ServiceKey("web", "web"), &ServiceRecord{
		Name: "web", Cluster: "web", AssignPublicIP: "ENABLED",
	}))
	task := &TaskRecord{
		TaskID: "t-1", Cluster: "web", Group: serviceTaskGroup("web"),
		ENIID: "eni-1", ENIPrivateIP: "172.31.0.8",
	}
	require.NoError(t, putJSON(kv, TaskKey("web", "t-1"), task))

	svc.assignTaskPublicIP(context.Background(), kv, testAccountID, task)

	assert.Equal(t, []string{"eni-1"}, eips.allocated)
	assert.Equal(t, "192.0.2.50", task.ENIPublicIP)
	assert.Equal(t, "eipalloc-1", task.ENIEIPAllocationID)

	var persisted TaskRecord
	found, err := getJSON(kv, TaskKey("web", "t-1"), &persisted)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "192.0.2.50", persisted.ENIPublicIP)
}

func TestAssignTaskPublicIP_DisabledNoOp(t *testing.T) {
	svc, _, kv := serviceTestRig(t)
	eips := &stubEIPs{publicIP: "192.0.2.50", allocID: "eipalloc-1"}
	svc.eips = eips

	require.NoError(t, putJSON(kv, ServiceKey("web", "web"), &ServiceRecord{
		Name: "web", Cluster: "web", // AssignPublicIP unset
	}))
	task := &TaskRecord{
		TaskID: "t-2", Cluster: "web", Group: serviceTaskGroup("web"), ENIID: "eni-2",
	}

	svc.assignTaskPublicIP(context.Background(), kv, testAccountID, task)

	assert.Empty(t, eips.allocated)
	assert.Empty(t, task.ENIPublicIP)
}

func TestReleaseTaskPublicIP_ReleasesAndClears(t *testing.T) {
	svc, _, _ := serviceTestRig(t)
	eips := &stubEIPs{}
	svc.eips = eips

	task := &TaskRecord{TaskID: "t-3", Cluster: "web", ENIPublicIP: "192.0.2.50", ENIEIPAllocationID: "eipalloc-9"}
	svc.releaseTaskPublicIP(context.Background(), testAccountID, task)

	assert.Equal(t, []string{"eipalloc-9"}, eips.released)
	assert.Empty(t, task.ENIPublicIP)
	assert.Empty(t, task.ENIEIPAllocationID)
}

func TestReleaseTaskPublicIP_NoAllocationNoOp(t *testing.T) {
	svc, _, _ := serviceTestRig(t)
	eips := &stubEIPs{}
	svc.eips = eips

	task := &TaskRecord{TaskID: "t-4", Cluster: "web"}
	svc.releaseTaskPublicIP(context.Background(), testAccountID, task)

	assert.Empty(t, eips.released)
}
