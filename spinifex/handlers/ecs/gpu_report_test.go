package handlers_ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/handlers/ecs/bus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ReportTaskGPU merges the agent-reported device UUIDs onto the matching
// container(s) of an existing task record, by container name.
func TestReportTaskGPU_MergesOntoTaskContainers(t *testing.T) {
	svc, _ := newTestService(t)
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "t-1"), &TaskRecord{
		TaskID: "t-1", Cluster: "web",
		Containers: []ContainerState{
			{Name: "web", Status: "RUNNING"},
			{Name: "trainer", Status: "RUNNING"},
		},
	}))

	out, err := svc.ReportTaskGPU(context.Background(), &ReportTaskGPUInput{
		Cluster: "web", Task: "t-1",
		Containers: []ContainerGPUReport{{Name: "trainer", GPUIDs: []string{"GPU-aaa"}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "OK", out.Acknowledgment)

	var rec TaskRecord
	found, err := getJSON(t.Context(), kv, TaskKey("web", "t-1"), &rec)
	require.NoError(t, err)
	require.True(t, found)
	for _, c := range rec.Containers {
		if c.Name == "trainer" {
			assert.Equal(t, []string{"GPU-aaa"}, c.GPUIDs)
		} else {
			assert.Empty(t, c.GPUIDs)
		}
	}
}

// An unknown task is a silent no-op (still acknowledged) — the state-report
// path already owns the task's lifecycle.
func TestReportTaskGPU_UnknownTaskNoop(t *testing.T) {
	svc, _ := newTestService(t)
	out, err := svc.ReportTaskGPU(context.Background(), &ReportTaskGPUInput{
		Cluster: "web", Task: "t-missing",
		Containers: []ContainerGPUReport{{Name: "trainer", GPUIDs: []string{"GPU-aaa"}}},
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "OK", out.Acknowledgment)
}

// DescribeTasks surfaces the reported UUIDs as the real AWS Container.gpuIds
// field, once ReportTaskGPU has merged them onto the task record.
func TestDescribeTasks_GPUIDsPopulatedFromReport(t *testing.T) {
	svc, _ := newTestService(t)
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "t-1"), &TaskRecord{
		TaskID: "t-1", ARN: TaskARN(svc.region, testAccountID, "web", "t-1"), Cluster: "web",
		Containers: []ContainerState{{Name: "trainer", Status: "RUNNING", GPUIDs: []string{"GPU-aaa", "GPU-bbb"}}},
	}))

	dt, err := svc.DescribeTasks(context.Background(), &ecs.DescribeTasksInput{
		Cluster: aws.String("web"), Tasks: []*string{aws.String("t-1")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, dt.Tasks, 1)
	require.Len(t, dt.Tasks[0].Containers, 1)
	assert.Equal(t, []string{"GPU-aaa", "GPU-bbb"}, awsStringSlice(dt.Tasks[0].Containers[0].GpuIds))
}

// recordTaskState must not wipe a container's pinned GPU UUIDs when a later
// state-change report omits them — the STOPPED report for a fast one-shot GPU
// task carries no GPUIDs, and DescribeTasks must still surface the UUIDs the
// RUNNING report (or ReportTaskGPU) previously set.
func TestRecordTaskState_PreservesGPUIDsAcrossStoppedReport(t *testing.T) {
	svc, _ := newTestService(t)
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "t-1"), &TaskRecord{
		TaskID: "t-1", ARN: TaskARN(svc.region, testAccountID, "web", "t-1"), Cluster: "web",
		ContainerInstanceID: "i-1",
		Containers:          []ContainerState{{Name: "trainer", Status: "PENDING"}},
	}))

	// RUNNING report pins the GPU UUIDs onto the container.
	require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: "t-1",
		LastStatus: bus.TaskStatusRunning,
		Containers: []bus.ContainerStatus{{Name: "trainer", Status: bus.TaskStatusRunning, GPUIDs: []string{"GPU-aaa", "GPU-bbb"}}},
	}))

	var rec TaskRecord
	found, err := getJSON(t.Context(), kv, TaskKey("web", "t-1"), &rec)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, rec.Containers, 1)
	assert.Equal(t, []string{"GPU-aaa", "GPU-bbb"}, rec.Containers[0].GPUIDs)

	// STOPPED report arrives without GPUIDs (the agent doesn't re-send them) —
	// the previously pinned UUIDs must survive the transition.
	exit := 0
	require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: "t-1",
		LastStatus: bus.TaskStatusStopped, Reason: "exited",
		Containers: []bus.ContainerStatus{{Name: "trainer", Status: bus.TaskStatusStopped, ExitCode: &exit}},
	}))

	found, err = getJSON(t.Context(), kv, TaskKey("web", "t-1"), &rec)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, rec.Containers, 1)
	assert.Equal(t, []string{"GPU-aaa", "GPU-bbb"}, rec.Containers[0].GPUIDs,
		"gpuIds must survive the STOPPED transition so DescribeTasks still reports them")

	// DescribeTasks still surfaces the preserved UUIDs after STOPPED.
	dt, err := svc.DescribeTasks(context.Background(), &ecs.DescribeTasksInput{
		Cluster: aws.String("web"), Tasks: []*string{aws.String("t-1")},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, dt.Tasks, 1)
	require.Len(t, dt.Tasks[0].Containers, 1)
	assert.Equal(t, []string{"GPU-aaa", "GPU-bbb"}, awsStringSlice(dt.Tasks[0].Containers[0].GpuIds))
}

// A state-change report that DOES carry GPUIDs still overwrites/replaces the
// pinned set normally (e.g. a corrected report, or GPUs reassigned on a retry).
func TestRecordTaskState_ReportedGPUIDsOverwritePinned(t *testing.T) {
	svc, _ := newTestService(t)
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "t-1"), &TaskRecord{
		TaskID: "t-1", ARN: TaskARN(svc.region, testAccountID, "web", "t-1"), Cluster: "web",
		ContainerInstanceID: "i-1",
		Containers:          []ContainerState{{Name: "trainer", Status: "RUNNING", GPUIDs: []string{"GPU-aaa"}}},
	}))

	require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: "t-1",
		LastStatus: bus.TaskStatusRunning,
		Containers: []bus.ContainerStatus{{Name: "trainer", Status: bus.TaskStatusRunning, GPUIDs: []string{"GPU-ccc"}}},
	}))

	var rec TaskRecord
	found, err := getJSON(t.Context(), kv, TaskKey("web", "t-1"), &rec)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, rec.Containers, 1)
	assert.Equal(t, []string{"GPU-ccc"}, rec.Containers[0].GPUIDs)
}

// A non-GPU container's GPUIDs stay empty across state transitions — there is
// nothing to preserve, and the fallback must not fabricate a value.
func TestRecordTaskState_NonGPUContainerStaysEmpty(t *testing.T) {
	svc, _ := newTestService(t)
	kv, err := svc.bucket(t.Context(), testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(t.Context(), kv, TaskKey("web", "t-1"), &TaskRecord{
		TaskID: "t-1", ARN: TaskARN(svc.region, testAccountID, "web", "t-1"), Cluster: "web",
		ContainerInstanceID: "i-1",
		Containers:          []ContainerState{{Name: "web", Status: "PENDING"}},
	}))

	require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: "t-1",
		LastStatus: bus.TaskStatusRunning,
		Containers: []bus.ContainerStatus{{Name: "web", Status: bus.TaskStatusRunning}},
	}))

	exit := 0
	require.NoError(t, svc.recordTaskState(context.Background(), &bus.TaskState{
		AccountID: testAccountID, ClusterName: "web", InstanceID: "i-1", TaskID: "t-1",
		LastStatus: bus.TaskStatusStopped,
		Containers: []bus.ContainerStatus{{Name: "web", Status: bus.TaskStatusStopped, ExitCode: &exit}},
	}))

	var rec TaskRecord
	found, err := getJSON(t.Context(), kv, TaskKey("web", "t-1"), &rec)
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, rec.Containers, 1)
	assert.Empty(t, rec.Containers[0].GPUIDs)
}
