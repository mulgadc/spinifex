package handlers_ecs

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubENI is an in-memory eniController for the awsvpc unit tests.
type stubENI struct {
	allocCalls   int
	attachCalls  int
	releaseCalls int
	allocErr     error
	attachErr    error
	lastSubnet   string
	lastSGs      []string
	released     []string
}

func (s *stubENI) Allocate(_, subnetID string, sgs []*string) (eniAllocation, error) {
	s.allocCalls++
	s.lastSubnet = subnetID
	for _, g := range sgs {
		s.lastSGs = append(s.lastSGs, aws.StringValue(g))
	}
	if s.allocErr != nil {
		return eniAllocation{}, s.allocErr
	}
	return eniAllocation{
		ENIID:      "eni-stub",
		MacAddress: "02:aa:bb:cc:dd:ee",
		PrivateIP:  "172.31.0.50",
		SubnetID:   subnetID,
	}, nil
}

func (s *stubENI) Attach(_, _, _ string) (string, error) {
	s.attachCalls++
	if s.attachErr != nil {
		return "", s.attachErr
	}
	return "eni-attach-stub", nil
}

func (s *stubENI) Release(_ string, rec *TaskRecord) error {
	s.releaseCalls++
	s.released = append(s.released, rec.ENIID)
	return nil
}

func registerAwsvpcTaskDef(t *testing.T, svc *Service, family string, cpu, mem int) {
	t.Helper()
	_, err := svc.RegisterTaskDefinition(&ecs.RegisterTaskDefinitionInput{
		Family:      aws.String(family),
		NetworkMode: aws.String(NetworkModeAwsvpc),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name:      aws.String("app"),
			Image:     aws.String("registry/app:1"),
			Cpu:       aws.Int64(int64(cpu)),
			Memory:    aws.Int64(int64(mem)),
			Essential: aws.Bool(true),
		}},
	}, testAccountID)
	require.NoError(t, err)
}

func awsvpcRunInput() *ecs.RunTaskInput {
	return &ecs.RunTaskInput{
		Cluster:        aws.String("web"),
		TaskDefinition: aws.String("app"),
		Count:          aws.Int64(1),
		NetworkConfiguration: &ecs.NetworkConfiguration{
			AwsvpcConfiguration: &ecs.AwsVpcConfiguration{
				Subnets:        []*string{aws.String("subnet-1")},
				SecurityGroups: []*string{aws.String("sg-1")},
			},
		},
	}
}

func TestRunTask_Awsvpc_AllocatesAttachesAssigns(t *testing.T) {
	svc, nc := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	sub, err := nc.SubscribeSync(bus.AssignSubject(testAccountID, "web", "i-1"))
	require.NoError(t, err)

	out, err := svc.RunTask(awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)
	assert.Empty(t, out.Failures)
	assert.Equal(t, 1, eni.allocCalls)
	assert.Equal(t, 1, eni.attachCalls)
	assert.Equal(t, "subnet-1", eni.lastSubnet)
	assert.Contains(t, eni.lastSGs, "sg-1")

	// Attachment surfaced on DescribeTasks.
	require.Len(t, out.Tasks[0].Attachments, 1)
	att := out.Tasks[0].Attachments[0]
	assert.Equal(t, "ElasticNetworkInterface", aws.StringValue(att.Type))
	assert.Equal(t, "eni-attach-stub", aws.StringValue(att.Id))
	assert.Equal(t, "eni-stub", detailValue(att, "networkInterfaceId"))
	assert.Equal(t, "172.31.0.50", detailValue(att, "privateIPv4Address"))

	// ENI plumbed into the assign payload.
	msg, err := sub.NextMsg(2 * time.Second)
	require.NoError(t, err)
	var as bus.Assign
	require.NoError(t, json.Unmarshal(msg.Data, &as))
	assert.Equal(t, "eni-stub", as.ENIID)
	assert.Equal(t, "02:aa:bb:cc:dd:ee", as.ENIMacAddress)
	assert.Equal(t, "172.31.0.50", as.ENIPrivateIP)
	assert.Equal(t, "subnet-1", as.ENISubnetID)
}

func TestRunTask_Awsvpc_NoSubnets_Errors(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	// awsvpc task def with no networkConfiguration → request rejected, no ENI.
	_, err = svc.RunTask(&ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, 0, eni.allocCalls)
}

func TestRunTask_Awsvpc_AttachFailure_RollsBack(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{attachErr: errors.New("hot-plug timeout")}
	svc.eni = eni
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Tasks)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, "RESOURCE:eni", aws.StringValue(out.Failures[0].Reason))

	// ENI was allocated then released; reservation rolled back.
	assert.Equal(t, 1, eni.allocCalls)
	assert.Equal(t, 1, eni.releaseCalls)
	assert.Contains(t, eni.released, "eni-stub")

	di, err := svc.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), aws.Int64Value(di.ContainerInstances[0].RunningTasksCount))
}

func TestRecordTaskState_Awsvpc_ReleasesENIOnStopOnce(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	out, err := svc.RunTask(awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	taskID := containerInstanceShortID(aws.StringValue(out.Tasks[0].TaskArn))

	stop := &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: taskID,
		LastStatus: bus.TaskStatusStopped, Reason: "exited",
	}
	require.NoError(t, svc.recordTaskState(stop))
	require.NoError(t, svc.recordTaskState(stop)) // re-report STOPPED

	// Released exactly once (the prev!=STOPPED guard makes the re-report a no-op).
	assert.Equal(t, 1, eni.releaseCalls)
	assert.Equal(t, []string{"eni-stub"}, eni.released)
}

func TestReaper_Awsvpc_ReleasesENI(t *testing.T) {
	svc, nc := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	_, err = svc.RunTask(awsvpcRunInput(), testAccountID)
	require.NoError(t, err)

	kv, err := svc.bucket(testAccountID)
	require.NoError(t, err)
	var rec InstanceRecord
	_, err = getJSON(kv, InstanceKey("web", "i-1"), &rec)
	require.NoError(t, err)
	rec.LastSeen = time.Now().UTC().Add(-2 * heartbeatTimeout)
	require.NoError(t, putJSON(kv, InstanceKey("web", "i-1"), &rec))

	sc := NewScheduler(nc, svc, "test-holder")
	sc.reapBucket(kv, testAccountID, time.Now().UTC())

	assert.Equal(t, 1, eni.releaseCalls)
	assert.Contains(t, eni.released, "eni-stub")
}

func TestRunTask_BridgeMode_NoENI(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(&ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256) // no networkMode → bridge default
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(&ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)
	assert.Equal(t, 0, eni.allocCalls)
	assert.Empty(t, out.Tasks[0].Attachments)
}

func TestResolveNetworkMode(t *testing.T) {
	assert.Equal(t, NetworkModeAwsvpc, resolveNetworkMode(&TaskDefRecord{NetworkMode: "awsvpc"}))
	assert.Equal(t, NetworkModeAwsvpc, resolveNetworkMode(&TaskDefRecord{NetworkMode: "AWSVPC"}))
	assert.Equal(t, NetworkModeHost, resolveNetworkMode(&TaskDefRecord{NetworkMode: "host"}))
	assert.Equal(t, NetworkModeBridge, resolveNetworkMode(&TaskDefRecord{})) // default
	assert.Equal(t, NetworkModeBridge, resolveNetworkMode(&TaskDefRecord{NetworkMode: "garbage"}))
}

func TestParseAwsvpcConfig(t *testing.T) {
	in := awsvpcRunInput()
	cfg, err := parseAwsvpcConfig(in, NetworkModeAwsvpc)
	require.NoError(t, err)
	assert.Equal(t, "subnet-1", cfg.firstSubnet())
	assert.Equal(t, []string{"sg-1"}, cfg.SecurityGroups)

	// awsvpc with no subnets is an error.
	_, err = parseAwsvpcConfig(&ecs.RunTaskInput{}, NetworkModeAwsvpc)
	require.Error(t, err)

	// bridge with no config is fine (empty).
	cfg, err = parseAwsvpcConfig(&ecs.RunTaskInput{}, NetworkModeBridge)
	require.NoError(t, err)
	assert.Empty(t, cfg.Subnets)
}

// respond subscribes to subject and replies with the JSON of whatever fn returns
// for each request, modelling the EC2/daemon handlers the controller calls.
func respond(t *testing.T, nc *nats.Conn, subject string, fn func([]byte) any) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		data, _ := json.Marshal(fn(msg.Data))
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func TestNATSENIController_AllocateAttachRelease(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	c := newNATSENIController(nc)

	respond(t, nc, "ec2.CreateNetworkInterface", func([]byte) any {
		return ec2.CreateNetworkInterfaceOutput{NetworkInterface: &ec2.NetworkInterface{
			NetworkInterfaceId: aws.String("eni-real"),
			MacAddress:         aws.String("02:11:22:33:44:55"),
			PrivateIpAddress:   aws.String("172.31.5.9"),
		}}
	})
	respond(t, nc, "ec2.cmd.i-1", func(req []byte) any {
		var cmd types.EC2InstanceCommand
		_ = json.Unmarshal(req, &cmd)
		if cmd.Attributes.AttachENI {
			return ec2.AttachNetworkInterfaceOutput{AttachmentId: aws.String("att-real")}
		}
		return ec2.DetachNetworkInterfaceOutput{}
	})
	respond(t, nc, "ec2.DeleteNetworkInterface", func([]byte) any {
		return ec2.DeleteNetworkInterfaceOutput{}
	})

	alloc, err := c.Allocate(testAccountID, "subnet-1", []*string{aws.String("sg-1")})
	require.NoError(t, err)
	assert.Equal(t, "eni-real", alloc.ENIID)
	assert.Equal(t, "02:11:22:33:44:55", alloc.MacAddress)
	assert.Equal(t, "172.31.5.9", alloc.PrivateIP)

	attID, err := c.Attach(testAccountID, "i-1", "eni-real")
	require.NoError(t, err)
	assert.Equal(t, "att-real", attID)

	require.NoError(t, c.Release(testAccountID, &TaskRecord{
		ENIID: "eni-real", ENIAttachmentID: "att-real", ContainerInstanceID: "i-1",
	}))
}

func TestNATSENIController_Release_NotFoundIsSuccess(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	c := newNATSENIController(nc)

	respond(t, nc, "ec2.cmd.i-1", func([]byte) any {
		return json.RawMessage(utils.GenerateErrorPayload("InvalidAttachmentID.NotFound"))
	})
	respond(t, nc, "ec2.DeleteNetworkInterface", func([]byte) any {
		return json.RawMessage(utils.GenerateErrorPayload("InvalidNetworkInterfaceID.NotFound"))
	})

	// Both legs report already-gone; Release converges without error.
	require.NoError(t, c.Release(testAccountID, &TaskRecord{
		ENIID: "eni-x", ENIAttachmentID: "att-x", ContainerInstanceID: "i-1",
	}))
}

func TestNATSENIController_Release_NoENINoop(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	c := newNATSENIController(nc)
	require.NoError(t, c.Release(testAccountID, &TaskRecord{})) // no ENIID
	require.NoError(t, c.Release(testAccountID, nil))
}

func TestIsENINotFound(t *testing.T) {
	assert.True(t, isENINotFound(errors.New("InvalidNetworkInterfaceID.NotFound")))
	assert.True(t, isENINotFound(errors.New("delete: InvalidAttachmentID.NotFound")))
	assert.False(t, isENINotFound(errors.New("ServerInternal")))
	assert.False(t, isENINotFound(nil))
}

func detailValue(att *ecs.Attachment, name string) string {
	for _, d := range att.Details {
		if aws.StringValue(d.Name) == name {
			return aws.StringValue(d.Value)
		}
	}
	return ""
}
