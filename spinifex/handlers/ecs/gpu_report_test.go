package handlers_ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ReportTaskGPU merges the agent-reported device UUIDs onto the matching
// container(s) of an existing task record, by container name.
func TestReportTaskGPU_MergesOntoTaskContainers(t *testing.T) {
	svc, _ := newTestService(t)
	kv, err := svc.bucket(testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(kv, TaskKey("web", "t-1"), &TaskRecord{
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
	found, err := getJSON(kv, TaskKey("web", "t-1"), &rec)
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
	kv, err := svc.bucket(testAccountID)
	require.NoError(t, err)
	require.NoError(t, putJSON(kv, TaskKey("web", "t-1"), &TaskRecord{
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
