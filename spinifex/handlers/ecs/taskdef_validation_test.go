package handlers_ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterTaskDefinition_RejectsSecrets verifies that a container secrets[]
// is hard-rejected rather than silently dropped.
func TestRegisterTaskDefinition_RejectsSecrets(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			Secrets: []*ecs.Secret{{
				Name: aws.String("DB_PASSWORD"), ValueFrom: aws.String("arn:aws:ssm:::parameter/db"),
			}},
		}},
	}, testAccountID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterException")
}

// TestRegisterTaskDefinition_AcceptsUnsupportedLogDriver verifies that a
// non-json-file driver is accepted for parity (warned, not rejected) and the
// driver round-trips through DescribeTaskDefinition.
func TestRegisterTaskDefinition_AcceptsUnsupportedLogDriver(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			LogConfiguration: &ecs.LogConfiguration{
				LogDriver: aws.String("awslogs"),
				Options:   map[string]*string{"awslogs-group": aws.String("/ecs/app")},
			},
		}},
	}, testAccountID)
	require.NoError(t, err)

	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String("app"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, d.TaskDefinition.ContainerDefinitions, 1)
	lc := d.TaskDefinition.ContainerDefinitions[0].LogConfiguration
	require.NotNil(t, lc)
	assert.Equal(t, "awslogs", aws.StringValue(lc.LogDriver))
	assert.Equal(t, "/ecs/app", aws.StringValue(lc.Options["awslogs-group"]))
}

// TestRegisterTaskDefinition_EchoesRequiresCompatibilities: only the EC2 launch
// type is implemented, but the field still has to round-trip. Returning an empty
// list to a client that sent ["EC2"] reads as drift, and Terraform treats it as
// a forced replacement on every plan.
func TestRegisterTaskDefinition_EchoesRequiresCompatibilities(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family:                  aws.String("app"),
		RequiresCompatibilities: aws.StringSlice([]string{"EC2"}),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
		}},
	}, testAccountID)
	require.NoError(t, err)

	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String("app"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"EC2"}, aws.StringValueSlice(d.TaskDefinition.RequiresCompatibilities))
}

// TestRegisterTaskDefinition_EchoesRuntimePlatform: runtimePlatform is inert
// (nothing selects capacity by CPU architecture or OS family in v1), but the
// same drift trap as RequiresCompatibilities applies if it is not echoed back.
func TestRegisterTaskDefinition_EchoesRuntimePlatform(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		RuntimePlatform: &ecs.RuntimePlatform{
			CpuArchitecture:       aws.String("ARM64"),
			OperatingSystemFamily: aws.String("LINUX"),
		},
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
		}},
	}, testAccountID)
	require.NoError(t, err)

	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String("app"),
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, d.TaskDefinition.RuntimePlatform)
	assert.Equal(t, "ARM64", aws.StringValue(d.TaskDefinition.RuntimePlatform.CpuArchitecture))
	assert.Equal(t, "LINUX", aws.StringValue(d.TaskDefinition.RuntimePlatform.OperatingSystemFamily))
}

// TestRegisterTaskDefinition_RuntimePlatformOmittedWhenUnset verifies that a
// task definition registered without runtimePlatform describes with a nil
// RuntimePlatform rather than an invented empty struct.
func TestRegisterTaskDefinition_RuntimePlatformOmittedWhenUnset(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
		}},
	}, testAccountID)
	require.NoError(t, err)

	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String("app"),
	}, testAccountID)
	require.NoError(t, err)
	assert.Nil(t, d.TaskDefinition.RuntimePlatform)
}

// TestRunTask_AssignCarriesExecutionRoleAndLogDriver verifies that execution
// role plumbed to the agent) and the log driver reaches the assign.
func TestRunTask_AssignCarriesExecutionRoleAndLogDriver(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	execARN := "arn:aws:iam::123456789012:role/exec-app"
	_, err = svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family:           aws.String("app"),
		ExecutionRoleArn: aws.String(execARN),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"),
			Cpu: aws.Int64(128), Memory: aws.Int64(256), Essential: aws.Bool(true),
			LogConfiguration: &ecs.LogConfiguration{LogDriver: aws.String(LogDriverJSONFile)},
		}},
	}, testAccountID)
	require.NoError(t, err)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String("app")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, execARN, aws.StringValue(d.TaskDefinition.ExecutionRoleArn))

	_, err = svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)

	poll, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, poll.Assignments, 1)
	assert.Equal(t, execARN, poll.Assignments[0].ExecutionRoleARN)
	require.Len(t, poll.Assignments[0].Containers, 1)
	assert.Equal(t, LogDriverJSONFile, poll.Assignments[0].Containers[0].LogDriver)
}

// TestRunTask_AssignCarriesGPU verifies that a
// resourceRequirements GPU count on the task def is threaded end-to-end —
// conv -> ContainerDef -> task-level aggregate (TaskRecord.GPU) -> the
// AssignContainer the agent polls for.
func TestRunTask_AssignCarriesGPU(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	_, err = svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("gpu-app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{
			{
				Name: aws.String("trainer"), Image: aws.String("registry/trainer:1"),
				Cpu: aws.Int64(512), Memory: aws.Int64(1024), Essential: aws.Bool(true),
				ResourceRequirements: []*ecs.ResourceRequirement{
					{Type: aws.String(ecs.ResourceTypeGpu), Value: aws.String("1")},
				},
			},
			{
				Name: aws.String("sidecar"), Image: aws.String("registry/sidecar:1"),
				Cpu: aws.Int64(64), Memory: aws.Int64(128), Essential: aws.Bool(false),
			},
		},
	}, testAccountID)
	require.NoError(t, err)
	registerInstanceGPU(t, svc, "web", "i-1", 1024, 2048, 1)

	out, err := svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("gpu-app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)

	poll, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, poll.Assignments, 1)
	require.Len(t, poll.Assignments[0].Containers, 2)
	assert.Equal(t, 1, poll.Assignments[0].Containers[0].GPU)
	assert.Zero(t, poll.Assignments[0].Containers[1].GPU)

	taskID := TaskShortID(aws.StringValue(out.Tasks[0].TaskArn))
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	var rec TaskRecord
	found, err := getJSON(t.Context(), kv, TaskKey("web", taskID), &rec)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 1, rec.GPU)
}

// The enforced container-runtime fields read back exactly as submitted: the
// read-back is the fix, not just acceptance at register time.
func TestRegisterTaskDefinition_EchoesRuntimeFields(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			User:                   aws.String("1000:1000"),
			ReadonlyRootFilesystem: aws.Bool(true),
			Privileged:             aws.Bool(false),
			PseudoTerminal:         aws.Bool(true),
			Interactive:            aws.Bool(true),
			StartTimeout:           aws.Int64(30),
			StopTimeout:            aws.Int64(0),
			SystemControls: []*ecs.SystemControl{
				{Namespace: aws.String("net.core.somaxconn"), Value: aws.String("1024")},
			},
			LinuxParameters: &ecs.LinuxParameters{
				Capabilities: &ecs.KernelCapabilities{
					Add:  aws.StringSlice([]string{"SYS_PTRACE"}),
					Drop: aws.StringSlice([]string{"NET_RAW"}),
				},
			},
		}},
	}, testAccountID)
	require.NoError(t, err)

	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String("app"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, d.TaskDefinition.ContainerDefinitions, 1)
	c := d.TaskDefinition.ContainerDefinitions[0]

	assert.Equal(t, "1000:1000", aws.StringValue(c.User))
	require.NotNil(t, c.ReadonlyRootFilesystem)
	assert.True(t, aws.BoolValue(c.ReadonlyRootFilesystem))
	require.NotNil(t, c.Privileged)
	assert.False(t, aws.BoolValue(c.Privileged))
	require.NotNil(t, c.PseudoTerminal)
	assert.True(t, aws.BoolValue(c.PseudoTerminal))
	require.NotNil(t, c.Interactive)
	assert.True(t, aws.BoolValue(c.Interactive))
	require.NotNil(t, c.StartTimeout)
	assert.EqualValues(t, 30, aws.Int64Value(c.StartTimeout))
	require.NotNil(t, c.StopTimeout)
	assert.EqualValues(t, 0, aws.Int64Value(c.StopTimeout))
	require.Len(t, c.SystemControls, 1)
	assert.Equal(t, "net.core.somaxconn", aws.StringValue(c.SystemControls[0].Namespace))
	assert.Equal(t, "1024", aws.StringValue(c.SystemControls[0].Value))
	require.NotNil(t, c.LinuxParameters)
	require.NotNil(t, c.LinuxParameters.Capabilities)
	assert.Equal(t, []string{"SYS_PTRACE"}, aws.StringValueSlice(c.LinuxParameters.Capabilities.Add))
	assert.Equal(t, []string{"NET_RAW"}, aws.StringValueSlice(c.LinuxParameters.Capabilities.Drop))
}

// A task definition registered without any of the new fields reads back
// byte-identical to before, so an existing revision does not churn.
func TestRegisterTaskDefinition_RuntimeFieldsOmittedWhenUnset(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
		}},
	}, testAccountID)
	require.NoError(t, err)

	d, err := svc.DescribeTaskDefinition(context.Background(), &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String("app"),
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, d.TaskDefinition.ContainerDefinitions, 1)
	c := d.TaskDefinition.ContainerDefinitions[0]

	assert.Empty(t, aws.StringValue(c.User))
	assert.Nil(t, c.ReadonlyRootFilesystem)
	assert.Nil(t, c.Privileged)
	assert.Nil(t, c.PseudoTerminal)
	assert.Nil(t, c.Interactive)
	assert.Nil(t, c.StartTimeout)
	assert.Nil(t, c.StopTimeout)
	assert.Empty(t, c.SystemControls)
	assert.Nil(t, c.LinuxParameters)
}

// TestRegisterTaskDefinition_AcceptsEmptyRuntimeFields is the case that keeps
// the module this fix exists to unblock working: an over-eager validator that
// refuses mountPoints=[] would break the terraform-aws-modules/ecs 5.12.1
// stack, which sends exactly these empty/false values for fields it does not
// otherwise use. Each requests nothing, so each must be accepted.
func TestRegisterTaskDefinition_AcceptsEmptyRuntimeFields(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			MountPoints:    []*ecs.MountPoint{},
			VolumesFrom:    []*ecs.VolumeFrom{},
			SystemControls: []*ecs.SystemControl{},
			LinuxParameters: &ecs.LinuxParameters{
				InitProcessEnabled: aws.Bool(false),
			},
		}},
	}, testAccountID)
	require.NoError(t, err)
}

// TestRegisterTaskDefinition_RejectsMountPoints verifies that a non-empty
// mountPoints is refused at registration rather than silently dropped: there
// is no task volume model to honor it against.
func TestRegisterTaskDefinition_RejectsMountPoints(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			MountPoints: []*ecs.MountPoint{{
				SourceVolume: aws.String("data"), ContainerPath: aws.String("/data"),
			}},
		}},
	}, testAccountID)
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorECSInvalidParameter, code)
	assert.Contains(t, message, "app")
	assert.Contains(t, message, "mountPoints")
}

// TestRegisterTaskDefinition_RejectsVolumesFrom mirrors RejectsMountPoints for
// volumesFrom: no task volume model, no way to honor it.
func TestRegisterTaskDefinition_RejectsVolumesFrom(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			VolumesFrom: []*ecs.VolumeFrom{{SourceContainer: aws.String("other")}},
		}},
	}, testAccountID)
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorECSInvalidParameter, code)
	assert.Contains(t, message, "volumesFrom")
}

// TestRegisterTaskDefinition_RejectsLinuxParametersDevices verifies device
// injection is refused rather than silently dropped.
func TestRegisterTaskDefinition_RejectsLinuxParametersDevices(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			LinuxParameters: &ecs.LinuxParameters{
				Devices: []*ecs.Device{{HostPath: aws.String("/dev/fuse")}},
			},
		}},
	}, testAccountID)
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorECSInvalidParameter, code)
	assert.Contains(t, message, "linuxParameters.devices")
}

// TestRegisterTaskDefinition_RejectsInitProcessEnabled verifies
// initProcessEnabled=true is refused: there is no init shim to honor it.
func TestRegisterTaskDefinition_RejectsInitProcessEnabled(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			LinuxParameters: &ecs.LinuxParameters{InitProcessEnabled: aws.Bool(true)},
		}},
	}, testAccountID)
	require.Error(t, err)
	code, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Equal(t, awserrors.ErrorECSInvalidParameter, code)
	assert.Contains(t, message, "initProcessEnabled")
}

// TestRegisterTaskDefinition_RejectsSharedMemoryAndTmpfs verifies
// sharedMemorySize and tmpfs are refused: both need mounts this agent does
// not model.
func TestRegisterTaskDefinition_RejectsSharedMemoryAndTmpfs(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("shm"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			LinuxParameters: &ecs.LinuxParameters{SharedMemorySize: aws.Int64(64)},
		}},
	}, testAccountID)
	require.Error(t, err)
	_, message, ok := awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Contains(t, message, "sharedMemorySize")

	_, err = svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("tmpfs"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			LinuxParameters: &ecs.LinuxParameters{
				Tmpfs: []*ecs.Tmpfs{{ContainerPath: aws.String("/tmp"), Size: aws.Int64(64)}},
			},
		}},
	}, testAccountID)
	require.Error(t, err)
	_, message, ok = awserrors.ResolveErrorDetail(err)
	require.True(t, ok)
	assert.Contains(t, message, "tmpfs")
}

// TestRunTask_AssignCarriesRuntimeFields verifies the enforced runtime fields
// reach bus.AssignContainer, the daemon-to-agent wire contract — the third of
// the four drop points this fix closes (stored, sent, applied, returned).
func TestRunTask_AssignCarriesRuntimeFields(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.CreateCluster(context.Background(), &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	_, err = svc.RegisterTaskDefinition(context.Background(), &ecs.RegisterTaskDefinitionInput{
		Family: aws.String("app"),
		ContainerDefinitions: []*ecs.ContainerDefinition{{
			Name: aws.String("app"), Image: aws.String("registry/app:1"), Essential: aws.Bool(true),
			User:                   aws.String("1000"),
			ReadonlyRootFilesystem: aws.Bool(true),
			Privileged:             aws.Bool(true),
			StartTimeout:           aws.Int64(15),
			StopTimeout:            aws.Int64(5),
			SystemControls: []*ecs.SystemControl{
				{Namespace: aws.String("net.core.somaxconn"), Value: aws.String("1024")},
			},
			LinuxParameters: &ecs.LinuxParameters{
				Capabilities: &ecs.KernelCapabilities{Add: aws.StringSlice([]string{"SYS_PTRACE"})},
			},
		}},
	}, testAccountID)
	require.NoError(t, err)
	registerInstance(t, svc, "web", "i-1", 1024, 2048)

	_, err = svc.RunTask(context.Background(), &ecs.RunTaskInput{
		Cluster: aws.String("web"), TaskDefinition: aws.String("app"), Count: aws.Int64(1),
	}, testAccountID)
	require.NoError(t, err)

	poll, err := svc.PollAssignments(context.Background(), &PollAssignmentsInput{Cluster: "web", ContainerInstance: "i-1"}, testAccountID)
	require.NoError(t, err)
	require.Len(t, poll.Assignments, 1)
	require.Len(t, poll.Assignments[0].Containers, 1)
	ac := poll.Assignments[0].Containers[0]

	assert.Equal(t, "1000", ac.User)
	require.NotNil(t, ac.ReadonlyRootFilesystem)
	assert.True(t, *ac.ReadonlyRootFilesystem)
	require.NotNil(t, ac.Privileged)
	assert.True(t, *ac.Privileged)
	require.NotNil(t, ac.StartTimeout)
	assert.EqualValues(t, 15, *ac.StartTimeout)
	require.NotNil(t, ac.StopTimeout)
	assert.EqualValues(t, 5, *ac.StopTimeout)
	require.Len(t, ac.SystemControls, 1)
	assert.Equal(t, "net.core.somaxconn", ac.SystemControls[0].Namespace)
	assert.Equal(t, []string{"SYS_PTRACE"}, ac.CapAdd)
}
