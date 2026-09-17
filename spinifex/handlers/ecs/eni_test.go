package handlers_ecs

import (
	"context"
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
	releaseErr   error
	lastSubnet   string
	lastSGs      []string
	released     []string
}

func (s *stubENI) Allocate(_ context.Context, _, subnetID string, sgs []*string) (eniAllocation, error) {
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

func (s *stubENI) Attach(_ context.Context, _, _, _ string) (string, error) {
	s.attachCalls++
	if s.attachErr != nil {
		return "", s.attachErr
	}
	return "eni-attach-stub", nil
}

func (s *stubENI) Release(_ context.Context, _ string, rec *TaskRecord) error {
	s.releaseCalls++
	if s.releaseErr != nil {
		return s.releaseErr
	}
	s.released = append(s.released, rec.ENIID)
	return nil
}

func registerAwsvpcTaskDef(t *testing.T, svc *Service, family string, cpu, mem int) {
	t.Helper()
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
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
	svc, _ := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
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

	// ENI plumbed into the assign payload, delivered via the instance KV inbox.
	poll, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, poll.Assignments, 1)
	as := poll.Assignments[0]
	assert.Equal(t, "eni-stub", as.ENIID)
	assert.Equal(t, "02:aa:bb:cc:dd:ee", as.ENIMacAddress)
	assert.Equal(t, "172.31.0.50", as.ENIPrivateIP)
	assert.Equal(t, "subnet-1", as.ENISubnetID)
}

func TestRunTask_Awsvpc_NoSubnets_Errors(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	// awsvpc task def with no networkConfiguration → request rejected, no ENI.
	_, err = svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, 0, eni.allocCalls)
}

func TestRunTask_Awsvpc_AttachFailure_RollsBack(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{attachErr: errors.New("hot-plug timeout")}
	svc.eni = eni
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	assert.Empty(t, out.Tasks)
	require.Len(t, out.Failures, 1)
	assert.Equal(t, "RESOURCE:ENI", aws.StringValue(out.Failures[0].Reason))
	// The caller is told which resource failed, not what went wrong inside the
	// server; the underlying error keeps its identifiers in the log.
	assert.NotContains(t, aws.StringValue(out.Failures[0].Detail), "hot-plug timeout")
	assert.NotContains(t, aws.StringValue(out.Failures[0].Detail), "eni-stub")

	// ENI was allocated then released; reservation rolled back.
	assert.Equal(t, 1, eni.allocCalls)
	assert.Equal(t, 1, eni.releaseCalls)
	assert.Contains(t, eni.released, "eni-stub")

	di, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), aws.Int64Value(di.ContainerInstances[0].RunningTasksCount))
}

// singleTaskRecord returns the sole task record in cluster, or found=false when
// none exists. Tests that never learn a taskID (a rolled-back RunTask returns
// no ARN) use this to reach the record straight from KV.
func singleTaskRecord(t *testing.T, svc *Service, cluster string) (TaskRecord, bool) {
	t.Helper()
	kv, err := svc.bucket(context.Background(), testAccountID)
	require.NoError(t, err)
	keys, err := keysWithPrefix(context.Background(), kv, TasksPrefix(cluster))
	require.NoError(t, err)
	require.LessOrEqual(t, len(keys), 1, "test expects at most one task record")
	if len(keys) == 0 {
		return TaskRecord{}, false
	}
	var rec TaskRecord
	found, err := getJSON(context.Background(), kv, keys[0], &rec)
	require.NoError(t, err)
	return rec, found
}

func TestReclaimTaskENI_ReleaseSucceeds_MarksReleased(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	task := &TaskRecord{TaskID: "t-1", NetworkMode: NetworkModeAwsvpc, ENIID: "eni-1", ENIAttachmentID: "att-1"}

	svc.reclaimTaskENI(context.Background(), testAccountID, task)

	assert.Equal(t, 1, eni.releaseCalls)
	assert.Equal(t, "eni-1", task.ENIID, "the identity survives release as the forensic record")
	assert.Equal(t, "att-1", task.ENIAttachmentID)
	assert.True(t, task.ENIReleased, "a successful release marks the task released")

	// A second call is a no-op: no repeat delete.
	svc.reclaimTaskENI(context.Background(), testAccountID, task)
	assert.Equal(t, 1, eni.releaseCalls, "an already-released record must not be released again")
}

func TestReclaimTaskENI_ReleaseFails_LeavesIdentity(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{releaseErr: errors.New("nats timeout")}
	svc.eni = eni
	task := &TaskRecord{TaskID: "t-1", NetworkMode: NetworkModeAwsvpc, ENIID: "eni-1", ENIAttachmentID: "att-1"}

	svc.reclaimTaskENI(context.Background(), testAccountID, task)

	assert.Equal(t, 1, eni.releaseCalls)
	assert.Equal(t, "eni-1", task.ENIID, "a failed release is still owed, so the identity must survive for the sweep")
	assert.Equal(t, "att-1", task.ENIAttachmentID)
	assert.False(t, task.ENIReleased, "a failed release must not be marked released")
}

func TestRunTask_Awsvpc_AttachFailure_ReleaseSucceeds_NoRecordPersisted(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{attachErr: errors.New("hot-plug timeout")}
	svc.eni = eni
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Failures, 1)

	_, found := singleTaskRecord(t, svc, "web")
	assert.False(t, found, "a release that succeeded leaves nothing for the sweep to find")
}

func TestRunTask_Awsvpc_AttachFailure_ReleaseFails_PersistsStoppedRecord(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{attachErr: errors.New("hot-plug timeout"), releaseErr: errors.New("release timeout")}
	svc.eni = eni
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Failures, 1)

	rec, found := singleTaskRecord(t, svc, "web")
	require.True(t, found, "a release that failed must leave a record so the sweep can retry it")
	assert.Equal(t, TaskStatusStopped, rec.LastStatus)
	assert.Equal(t, TaskStatusStopped, rec.DesiredStatus)
	assert.Equal(t, "eni-stub", rec.ENIID, "the ENI identity must survive so the sweep owns the retry")
	assert.False(t, rec.ENIReleased, "a failed release must not be marked released")
	assert.False(t, rec.StoppedAt.IsZero())

	// The reservation was already given back to the instance.
	di, err := svc.DescribeContainerInstances(context.Background(), &ecs.DescribeContainerInstancesInput{
		Cluster: aws.String("web"), ContainerInstances: []*string{aws.String("i-1")},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), aws.Int64Value(di.ContainerInstances[0].RunningTasksCount))
}

func TestRecordTaskState_Awsvpc_ReleasesENIOnStopOnce(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	out, err := svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)
	taskID := ContainerInstanceShortID(aws.StringValue(out.Tasks[0].TaskArn))

	stop := &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: taskID,
		LastStatus: bus.TaskStatusStopped, Reason: "exited",
	}
	require.NoError(t, svc.recordTaskState(context.Background(), stop))
	require.NoError(t, svc.recordTaskState(context.Background(), stop)) // re-report STOPPED

	// Released exactly once (the prev!=STOPPED guard makes the re-report a no-op).
	assert.Equal(t, 1, eni.releaseCalls)
	assert.Equal(t, []string{"eni-stub"}, eni.released)
}

func TestReaper_Awsvpc_ReleasesENI(t *testing.T) {
	svc, nc := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerAwsvpcTaskDef(t, svc, "app", 128, 256)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)
	_, err = svc.RunTask(context.Background(), awsvpcRunInput(), testAccountID)
	require.NoError(t, err)

	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec InstanceRecord
	_, err = getJSON(t.Context(), kv, InstanceKey("web", "i-1"), &rec)
	require.NoError(t, err)
	rec.LastSeen = time.Now().UTC().Add(-2 * heartbeatTimeout)
	require.NoError(t, putJSON(t.Context(), kv, InstanceKey("web", "i-1"), &rec))

	sc := NewScheduler(nc, svc, "test-holder")
	sc.reapBucket(context.Background(), kv, testAccountID, time.Now().UTC())

	assert.Equal(t, 1, eni.releaseCalls)
	assert.Contains(t, eni.released, "eni-stub")
}

func TestRunTask_BridgeMode_NoENI(t *testing.T) {
	svc, _ := newTestService(t)
	eni := &stubENI{}
	svc.eni = eni
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "app", 128, 256) // no networkMode → bridge default
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)
	assert.Equal(t, 0, eni.allocCalls)
	assert.Empty(t, out.Tasks[0].Attachments)
}

// TestTaskToAWS_StoppedAwsvpc_ReleasedENI_ReportsDeletedAttachment pins the
// regression this fix closes: a STOPPED awsvpc task whose ENI has already been
// released must still carry its ElasticNetworkInterface attachment, with
// Status DELETED rather than an empty attachments list.
func TestTaskToAWS_StoppedAwsvpc_ReleasedENI_ReportsDeletedAttachment(t *testing.T) {
	svc, _ := newTestService(t)
	rec := &TaskRecord{
		TaskID: "t-1", Cluster: "web", ARN: TaskARN(testRegion, testAccountID, "web", "t-1"),
		LastStatus: TaskStatusStopped, DesiredStatus: TaskStatusStopped,
		NetworkMode:     NetworkModeAwsvpc,
		ENIID:           "eni-1",
		ENIAttachmentID: "att-1",
		ENIPrivateIP:    "172.31.0.50",
		ENIReleased:     true,
	}

	task := svc.taskToAWS(testAccountID, rec)

	require.Len(t, task.Attachments, 1, "a released ENI must still be reported, not dropped")
	att := task.Attachments[0]
	assert.Equal(t, "ElasticNetworkInterface", aws.StringValue(att.Type))
	assert.Equal(t, "eni-1", detailValue(att, "networkInterfaceId"))
	assert.Equal(t, "172.31.0.50", detailValue(att, "privateIPv4Address"))
	assert.Equal(t, "DELETED", aws.StringValue(att.Status))
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

	alloc, err := c.Allocate(context.Background(), testAccountID, "subnet-1", []*string{aws.String("sg-1")})
	require.NoError(t, err)
	assert.Equal(t, "eni-real", alloc.ENIID)
	assert.Equal(t, "02:11:22:33:44:55", alloc.MacAddress)
	assert.Equal(t, "172.31.5.9", alloc.PrivateIP)

	attID, err := c.Attach(context.Background(), testAccountID, "i-1", "eni-real")
	require.NoError(t, err)
	assert.Equal(t, "att-real", attID)

	require.NoError(t, c.Release(context.Background(), testAccountID, &TaskRecord{
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
	require.NoError(t, c.Release(context.Background(), testAccountID, &TaskRecord{
		ENIID: "eni-x", ENIAttachmentID: "att-x", ContainerInstanceID: "i-1",
	}))
}

func TestNATSENIController_Release_NoENINoop(t *testing.T) {
	_, nc, _ := testutil.StartTestJetStream(t)
	c := newNATSENIController(nc)
	require.NoError(t, c.Release(context.Background(), testAccountID, &TaskRecord{})) // no ENIID
	require.NoError(t, c.Release(context.Background(), testAccountID, nil))
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
