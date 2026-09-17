// unexported, and the reservation they maintain is only observable from inside
// the package before a task reaches the API surface.
//
//test:in-package — placeTask and InstanceRecord's slot arithmetic are
package handlers_ecs

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerTypedInstance registers through the AWS path an agent uses, so the
// instance carries the ecs.instance-type attribute the ENI cap is derived from.
func registerTypedInstance(t *testing.T, svc *Service, cluster, id, instanceType string, cpu, mem int) {
	t.Helper()
	_, err := svc.RegisterContainerInstance(t.Context(), &ecs.RegisterContainerInstanceInput{
		Cluster:                  aws.String(cluster),
		InstanceIdentityDocument: aws.String(id),
		TotalResources: []*ecs.Resource{
			{Name: aws.String("CPU"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(cpu))},
			{Name: aws.String("MEMORY"), Type: aws.String("INTEGER"), IntegerValue: aws.Int64(int64(mem))},
		},
		Attributes: []*ecs.Attribute{
			{Name: aws.String(AttrInstanceType), Value: aws.String(instanceType)},
			{Name: aws.String(AttrAvailabilityZone), Value: aws.String("ap-southeast-2a")},
		},
	}, testAccountID)
	require.NoError(t, err)
}

func TestRegisterContainerInstance_RecordsInstanceTypeAttribute(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTypedInstance(t, svc, "web", "i-1", "t3.micro", 1024, 2048)

	out, err := svc.DescribeContainerInstances(t.Context(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.ContainerInstances, 1)
	assert.Equal(t, "t3.micro", attributeValue(out.ContainerInstances[0].Attributes, AttrInstanceType))
	assert.Equal(t, "ap-southeast-2a", attributeValue(out.ContainerInstances[0].Attributes, AttrAvailabilityZone))
}

func TestInstanceRecord_ENICapacityComesFromInstanceType(t *testing.T) {
	// A t3 carries three ENIs, one of which is the instance's own, so two task
	// ENIs fit.
	r := InstanceRecord{Status: InstanceStatusActive, InstanceType: "t3.micro", TotalCPU: 1024, TotalMemoryMiB: 2048}
	assert.Equal(t, 2, r.totalENIs())
	assert.True(t, r.fits(0, 0, 0, 2))
	assert.False(t, r.fits(0, 0, 0, 3))

	r.ReservedENIs = 2
	assert.True(t, r.fits(0, 0, 0, 0), "a task needing no ENI still fits a fully-committed instance")
	assert.False(t, r.fits(0, 0, 0, 1))
}

// An agent too old to report its type must not have every awsvpc placement
// refused; that would turn a mixed-version cluster into an outage.
func TestInstanceRecord_UnknownInstanceTypeIsNotENIGated(t *testing.T) {
	r := InstanceRecord{Status: InstanceStatusActive, TotalCPU: 1024, TotalMemoryMiB: 2048, ReservedENIs: 99}
	assert.True(t, r.fits(0, 0, 0, 1))
}

func TestPlaceTask_ENIExhaustionIsItsOwnRefusal(t *testing.T) {
	full := InstanceRecord{
		InstanceID: "i-1", Status: InstanceStatusActive, InstanceType: "t3.micro",
		TotalCPU: 1024, TotalMemoryMiB: 2048, ReservedENIs: 2,
	}
	_, err := placeTask([]InstanceRecord{full}, 128, 256, 0, 1, StrategyBinpack)
	require.ErrorIs(t, err, ErrNoENICapacity)

	// The same instance with no CPU left is a plain capacity refusal, so the two
	// are not collapsed into one message.
	full.ReservedCPU = 1024
	_, err = placeTask([]InstanceRecord{full}, 128, 256, 0, 1, StrategyBinpack)
	require.ErrorIs(t, err, ErrNoCapacity)
}

func TestRunTask_Awsvpc_ThirdTaskDeclinedAtPlacement(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerTypedInstance(t, svc, "web", "i-1", "t3.micro", 4096, 8192)

	out, err := svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)
	out, err = svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)

	// Two slots are committed. The CPU and memory are nowhere near exhausted, so
	// nothing but the interfaces can decline this one.
	allocsBefore := eni.allocCalls
	out, err = svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Tasks)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, "RESOURCE:ENI", aws.StringValue(out.Failures[0].Reason))

	// The point of moving the check to placement: no ENI is created for a task
	// that cannot be placed, so an over-subscribed service stops churning them.
	assert.Equal(t, allocsBefore, eni.allocCalls)

	// A capacity refusal is the reason alone, as AWS sends it, so a caller sees
	// no internal identifier and no server error code.
	assert.Nil(t, out.Failures[0].Detail)
}

func TestStartTask_Awsvpc_DeclinedWhenInstanceHasNoSlot(t *testing.T) {
	svc, _ := newTestService(t)
	svc.eni = &stubENI{}
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerTypedInstance(t, svc, "web", "i-1", "t3.micro", 4096, 8192)

	start := func() *ecs.StartTaskOutput {
		out, serr := svc.StartTask(context.Background(), &ecs.StartTaskInput{
			Cluster:            aws.String("web"),
			TaskDefinition:     aws.String("app"),
			ContainerInstances: []*string{aws.String("i-1")},
			NetworkConfiguration: &ecs.NetworkConfiguration{
				AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
					Subnets:        []*string{aws.String("subnet-1")},
					SecurityGroups: []*string{aws.String("sg-1")},
				},
			},
		}, testAccountID)
		require.NoError(t, serr)
		return out
	}

	require.Len(t, start().Tasks, 1)
	require.Len(t, start().Tasks, 1)
	out := start()
	assert.Empty(t, out.Tasks)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, "RESOURCE:ENI", aws.StringValue(out.Failures[0].Reason))
}

// A stopped task hands its slot back, or an instance would take exactly as many
// awsvpc tasks as it has slots for its whole life.
func TestTaskStop_ReleasesENIReservation(t *testing.T) {
	svc, _ := newTestService(t)
	svc.eni = &stubENI{}
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerTypedInstance(t, svc, "web", "i-1", "t3.micro", 4096, 8192)

	first, err := svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	require.Len(t, first.Tasks, 1)
	second, err := svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	require.Len(t, second.Tasks, 1)

	// The agent reporting STOPPED is what releases a reservation; a StopTask only
	// asks for it, and the release still waits on the report.
	require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
		AccountID:   testAccountID,
		ClusterName: "web",
		TaskID:      ContainerInstanceShortID(aws.StringValue(first.Tasks[0].TaskArn)),
		LastStatus:  TaskStatusStopped,
		Reason:      "test",
	}))

	third, err := svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	assert.Empty(t, third.Failures)
	assert.Len(t, third.Tasks, 1)
}

// The attach path stays as a backstop for a failure the reservation cannot
// predict, and it too keeps the server's own error out of the caller's reach.
func TestProvisionTaskENI_AttachFailureDetailIsNotTheServerError(t *testing.T) {
	svc, _ := newTestService(t)
	svc.eni = &stubENI{attachErr: errors.New("ServerInternal")}
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerTypedInstance(t, svc, "web", "i-1", "t3.micro", 4096, 8192)

	out, err := svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, "RESOURCE:ENI", aws.StringValue(out.Failures[0].Reason))
	assert.NotContains(t, aws.StringValue(out.Failures[0].Detail), "ServerInternal")

	// The reservation came back, so the failure did not cost the instance a slot.
	out, err = svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, "RESOURCE:ENI", aws.StringValue(out.Failures[0].Reason))
}
