package handlers_ecs

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reRegister drives the agent's heartbeat path: an idempotent
// RegisterContainerInstance through the gateway handler.
func reRegister(t *testing.T, svc *Service, cluster, id string) {
	t.Helper()
	_, err := svc.RegisterContainerInstance(&ecs.RegisterContainerInstanceInput{
		Cluster:                  aws.String(cluster),
		InstanceIdentityDocument: aws.String(id),
	}, testAccountID)
	require.NoError(t, err)
}

func instanceStatus(t *testing.T, svc *Service, cluster, id string) InstanceRecord {
	t.Helper()
	kv, err := svc.bucket(testAccountID)
	require.NoError(t, err)
	var rec InstanceRecord
	found, err := getJSON(kv, InstanceKey(cluster, id), &rec)
	require.NoError(t, err)
	require.True(t, found)
	return rec
}

// A reaper-drained (involuntary) instance returns to ACTIVE once its agent
// re-registers, so a control-plane restart that briefly reaps live instances
// self-heals instead of stranding them in DRAINING.
func TestRegister_RestoresReapedInstanceToActive(t *testing.T) {
	svc, nc := newTestService(t)
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	kv, err := svc.bucket(testAccountID)
	require.NoError(t, err)
	rec := instanceStatus(t, svc, "web", "i-1")
	rec.LastSeen = time.Now().UTC().Add(-2 * heartbeatTimeout)
	require.NoError(t, putJSON(kv, InstanceKey("web", "i-1"), &rec))

	NewScheduler(nc, svc, "test-holder").reapBucket(kv, testAccountID, time.Now().UTC())

	reaped := instanceStatus(t, svc, "web", "i-1")
	require.Equal(t, InstanceStatusDraining, reaped.Status)
	require.True(t, reaped.Reaped)

	reRegister(t, svc, "web", "i-1")

	recovered := instanceStatus(t, svc, "web", "i-1")
	assert.Equal(t, InstanceStatusActive, recovered.Status)
	assert.False(t, recovered.Reaped)
}

// An operator UpdateContainerInstancesState=DRAINING is intentional and must
// persist even though the live agent keeps re-registering.
func TestRegister_DoesNotUndoOperatorDrain(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	arn := instanceStatus(t, svc, "web", "i-1").ARN
	_, err = svc.UpdateContainerInstancesState(&ecs.UpdateContainerInstancesStateInput{
		Cluster:            aws.String("web"),
		ContainerInstances: []*string{aws.String(arn)},
		Status:             aws.String(InstanceStatusDraining),
	}, testAccountID)
	require.NoError(t, err)

	drained := instanceStatus(t, svc, "web", "i-1")
	require.Equal(t, InstanceStatusDraining, drained.Status)
	require.False(t, drained.Reaped)

	reRegister(t, svc, "web", "i-1")

	after := instanceStatus(t, svc, "web", "i-1")
	assert.Equal(t, InstanceStatusDraining, after.Status, "operator drain must survive agent re-register")
}
