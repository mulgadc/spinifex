package ecsv1_test

import (
	"encoding/json"
	"testing"
	"time"

	ecsv1 "github.com/mulgadc/spinifex/contracts/ecs/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var pinTime = time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)

func TestRegisterInstance_JSON(t *testing.T) {
	golden := `{"accountId":"123456789012","clusterName":"default","instanceId":"i-abc123",` +
		`"availabilityZone":"az-1","hostname":"host-1",` +
		`"capacity":{"cpu":2048,"memoryMiB":4096},"agentVersion":"1.0.0",` +
		`"registeredAt":"2026-01-15T10:30:00Z"}`
	msg := ecsv1.RegisterInstance{
		AccountID: "123456789012", ClusterName: "default", InstanceID: "i-abc123",
		AZ: "az-1", Hostname: "host-1",
		Capacity:     ecsv1.InstanceCapacity{CPU: 2048, MemoryMiB: 4096},
		AgentVersion: "1.0.0", RegisteredAt: pinTime,
	}

	data, err := json.Marshal(msg)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(data))

	var decoded ecsv1.RegisterInstance
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, msg, decoded)
}

func TestInstanceCapacity_GPUFields(t *testing.T) {
	golden := `{"cpu":4096,"memoryMiB":8192,"gpu":2,"gpuIds":["GPU-1","GPU-2"]}`
	capacity := ecsv1.InstanceCapacity{CPU: 4096, MemoryMiB: 8192, GPU: 2, GPUIDs: []string{"GPU-1", "GPU-2"}}

	data, err := json.Marshal(capacity)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(data))

	var decoded ecsv1.InstanceCapacity
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, capacity, decoded)
}

func TestHeartbeat_JSON(t *testing.T) {
	golden := `{"accountId":"123456789012","clusterName":"default","instanceId":"i-abc123",` +
		`"status":"ACTIVE","runningTasks":3,"sentAt":"2026-01-15T10:30:00Z"}`
	msg := ecsv1.Heartbeat{
		AccountID: "123456789012", ClusterName: "default", InstanceID: "i-abc123",
		Status: ecsv1.StatusActive, RunningTasks: 3, SentAt: pinTime,
	}

	data, err := json.Marshal(msg)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(data))

	var decoded ecsv1.Heartbeat
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, msg, decoded)
}

func TestStopDirective_JSON(t *testing.T) {
	golden := `{"taskId":"t-deadbeef","reason":"drain"}`
	msg := ecsv1.StopDirective{TaskID: "t-deadbeef", Reason: "drain"}

	data, err := json.Marshal(msg)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(data))

	var decoded ecsv1.StopDirective
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, msg, decoded)
}

func TestStopDirective_OmitsEmptyReason(t *testing.T) {
	data, err := json.Marshal(ecsv1.StopDirective{TaskID: "t-deadbeef"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"taskId":"t-deadbeef"}`, string(data))
}

func TestAssign_JSON(t *testing.T) {
	ro := true
	start := int64(30)
	golden := `{"accountId":"123456789012","clusterName":"default","instanceId":"i-abc123",` +
		`"taskId":"t-deadbeef","taskArn":"arn:aws:ecs:ap-southeast-2:123456789012:task/default/t-deadbeef",` +
		`"taskDefFamily":"web","taskDefRevision":2,` +
		`"containers":[{"name":"app","image":"registry/app:1","essential":true,` +
		`"readonlyRootFilesystem":true,"startTimeout":30}],` +
		`"eniId":"eni-1","eniMac":"02:00:00:00:00:01","eniPrivateIp":"10.0.1.5","eniSubnetId":"subnet-1",` +
		`"credId":"c-1","taskRoleArn":"arn:aws:iam::123456789012:role/task",` +
		`"executionRoleArn":"arn:aws:iam::123456789012:role/exec",` +
		`"assignedAt":"2026-01-15T10:30:00Z"}`
	msg := ecsv1.Assign{
		AccountID: "123456789012", ClusterName: "default", InstanceID: "i-abc123",
		TaskID: "t-deadbeef", TaskARN: "arn:aws:ecs:ap-southeast-2:123456789012:task/default/t-deadbeef",
		TaskDefFamily: "web", TaskDefRevision: 2,
		Containers: []ecsv1.AssignContainer{{
			Name: "app", Image: "registry/app:1", Essential: true,
			ReadonlyRootFilesystem: &ro, StartTimeout: &start,
		}},
		ENIID: "eni-1", ENIMacAddress: "02:00:00:00:00:01", ENIPrivateIP: "10.0.1.5", ENISubnetID: "subnet-1",
		CredID: "c-1", TaskRoleARN: "arn:aws:iam::123456789012:role/task",
		ExecutionRoleARN: "arn:aws:iam::123456789012:role/exec",
		AssignedAt:       pinTime,
	}

	data, err := json.Marshal(msg)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(data))

	var decoded ecsv1.Assign
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, msg, decoded)
}

func TestAssignContainer_FullFields(t *testing.T) {
	ro, priv, pty, interactive := true, true, false, true
	start, stop := int64(30), int64(10)
	golden := `{"name":"app","image":"registry/app:1","cpu":256,"memoryMiB":512,"gpu":1,` +
		`"essential":true,"command":["/bin/app"],"environment":{"ENV":"prod"},` +
		`"portMappings":[{"containerPort":80,"hostPort":8080,"protocol":"tcp","name":"http"}],` +
		`"logDriver":"json-file","user":"1000","readonlyRootFilesystem":true,"privileged":true,` +
		`"pseudoTerminal":false,"interactive":true,` +
		`"systemControls":[{"namespace":"net.core.somaxconn","value":"1024"}],` +
		`"capAdd":["SYS_PTRACE"],"capDrop":["NET_RAW"],"startTimeout":30,"stopTimeout":10}`
	msg := ecsv1.AssignContainer{
		Name: "app", Image: "registry/app:1", CPU: 256, MemoryMiB: 512, GPU: 1,
		Essential: true, Command: []string{"/bin/app"}, Environment: map[string]string{"ENV": "prod"},
		PortMappings:           []ecsv1.PortMapping{{ContainerPort: 80, HostPort: 8080, Protocol: "tcp", Name: "http"}},
		LogDriver:              "json-file",
		User:                   "1000",
		ReadonlyRootFilesystem: &ro,
		Privileged:             &priv,
		PseudoTerminal:         &pty,
		Interactive:            &interactive,
		SystemControls:         []ecsv1.SystemControl{{Namespace: "net.core.somaxconn", Value: "1024"}},
		CapAdd:                 []string{"SYS_PTRACE"},
		CapDrop:                []string{"NET_RAW"},
		StartTimeout:           &start,
		StopTimeout:            &stop,
	}

	data, err := json.Marshal(msg)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(data))

	var decoded ecsv1.AssignContainer
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, msg, decoded)
}

func TestAssignContainer_OmitsEmptyFields(t *testing.T) {
	data, err := json.Marshal(ecsv1.AssignContainer{Name: "app", Image: "registry/app:1"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"app","image":"registry/app:1","essential":false}`, string(data))
}

func TestTaskState_JSON(t *testing.T) {
	exit := 0
	golden := `{"accountId":"123456789012","clusterName":"default","instanceId":"i-abc123",` +
		`"taskId":"t-deadbeef","lastStatus":"STOPPED",` +
		`"containers":[{"name":"app","status":"STOPPED","containerId":"c-1","exitCode":0,"gpuIds":["GPU-1"]}],` +
		`"reason":"done","reportedAt":"2026-01-15T10:30:00Z"}`
	msg := ecsv1.TaskState{
		AccountID: "123456789012", ClusterName: "default", InstanceID: "i-abc123",
		TaskID: "t-deadbeef", LastStatus: ecsv1.TaskStatusStopped,
		Containers: []ecsv1.ContainerStatus{{
			Name: "app", Status: ecsv1.TaskStatusStopped, ContainerID: "c-1",
			ExitCode: &exit, GPUIDs: []string{"GPU-1"},
		}},
		Reason: "done", ReportedAt: pinTime,
	}

	data, err := json.Marshal(msg)
	require.NoError(t, err)
	assert.JSONEq(t, golden, string(data))

	var decoded ecsv1.TaskState
	require.NoError(t, json.Unmarshal([]byte(golden), &decoded))
	assert.Equal(t, msg, decoded)
}

func TestLifecycleStatusConstants(t *testing.T) {
	assert.Equal(t, "ACTIVE", ecsv1.StatusActive)
	assert.Equal(t, "DRAINING", ecsv1.StatusDraining)
	assert.Equal(t, "PENDING", ecsv1.TaskStatusPending)
	assert.Equal(t, "RUNNING", ecsv1.TaskStatusRunning)
	assert.Equal(t, "STOPPED", ecsv1.TaskStatusStopped)
}
