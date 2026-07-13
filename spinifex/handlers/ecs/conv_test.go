package handlers_ecs

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestARNBuilders(t *testing.T) {
	assert.Equal(t, "arn:aws:ecs:ap-southeast-2:123456789012:cluster/web",
		ClusterARN("ap-southeast-2", "123456789012", "web"))
	assert.Equal(t, "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/nginx:3",
		TaskDefARN("ap-southeast-2", "123456789012", "nginx", 3))
	assert.Equal(t, "arn:aws:ecs:ap-southeast-2:123456789012:task/web/t-1",
		TaskARN("ap-southeast-2", "123456789012", "web", "t-1"))
	assert.Equal(t, "arn:aws:ecs:ap-southeast-2:123456789012:container-instance/web/i-1",
		ContainerInstanceARN("ap-southeast-2", "123456789012", "web", "i-1"))
}

func TestTaskDefReservedSums(t *testing.T) {
	td := &TaskDefRecord{Containers: []ContainerDef{
		{CPU: 128, MemoryMiB: 256},
		{CPU: 64, MemoryMiB: 128},
	}}
	assert.Equal(t, 192, td.reservedCPU())
	assert.Equal(t, 384, td.reservedMemory())
}

func TestClusterShortName(t *testing.T) {
	assert.Equal(t, defaultCluster, clusterShortName(""))
	assert.Equal(t, "web", clusterShortName("web"))
	assert.Equal(t, "web", clusterShortName("arn:aws:ecs:r:a:cluster/web"))
}

func TestContainerInstanceShortID(t *testing.T) {
	assert.Equal(t, "i-1", containerInstanceShortID("i-1"))
	assert.Equal(t, "i-1", containerInstanceShortID("arn:aws:ecs:r:a:container-instance/web/i-1"))
}

func TestAWSStringSlice(t *testing.T) {
	got := awsStringSlice([]*string{aws.String("a"), nil, aws.String(""), aws.String("b")})
	assert.Equal(t, []string{"a", "b"}, got)
}

// containerDefsFromAWS → toAWS / toAssignContainer must preserve image, command,
// env, and port mappings (the fields the agent actually runs on).
func TestContainerDefRoundTrip(t *testing.T) {
	in := []*ecs.ContainerDefinition{{
		Name:         aws.String("web"),
		Image:        aws.String("registry/web:1"),
		Cpu:          aws.Int64(128),
		Memory:       aws.Int64(256),
		Essential:    aws.Bool(true),
		Command:      []*string{aws.String("/bin/sh"), aws.String("-c")},
		Environment:  []*ecs.KeyValuePair{{Name: aws.String("FOO"), Value: aws.String("bar")}},
		PortMappings: []*ecs.PortMapping{{ContainerPort: aws.Int64(80), HostPort: aws.Int64(8080), Protocol: aws.String("tcp")}},
	}}
	defs := containerDefsFromAWS(in)
	require.Len(t, defs, 1)
	d := defs[0]
	assert.Equal(t, "registry/web:1", d.Image)
	assert.Equal(t, 128, d.CPU)
	assert.Equal(t, []string{"/bin/sh", "-c"}, d.Command)
	assert.Equal(t, "bar", d.Environment["FOO"])
	require.Len(t, d.PortMappings, 1)
	assert.Equal(t, 8080, d.PortMappings[0].HostPort)

	back := d.toAWS()
	assert.Equal(t, "registry/web:1", aws.StringValue(back.Image))
	assert.Equal(t, int64(80), aws.Int64Value(back.PortMappings[0].ContainerPort))

	ac := d.toAssignContainer()
	assert.Equal(t, "web", ac.Name)
	assert.Equal(t, "registry/web:1", ac.Image)
	assert.Equal(t, "bar", ac.Environment["FOO"])
}

func TestContainerDefsFromAWS_SkipsNil(t *testing.T) {
	assert.Empty(t, containerDefsFromAWS([]*ecs.ContainerDefinition{nil}))
}

// TestContainerDefsFromAWS_GPU covers mulga-11opz (Epic C task C1): a
// resourceRequirements entry of type=GPU is parsed as a whole-GPU count and
// carried onto ContainerDef, its AWS round trip, and the bus AssignContainer.
func TestContainerDefsFromAWS_GPU(t *testing.T) {
	in := []*ecs.ContainerDefinition{{
		Name: aws.String("gpu-app"), Image: aws.String("registry/gpu-app:1"), Essential: aws.Bool(true),
		ResourceRequirements: []*ecs.ResourceRequirement{
			{Type: aws.String(ecs.ResourceTypeGpu), Value: aws.String("2")},
		},
	}}
	defs := containerDefsFromAWS(in)
	require.Len(t, defs, 1)
	assert.Equal(t, 2, defs[0].GPU)

	back := defs[0].toAWS()
	require.Len(t, back.ResourceRequirements, 1)
	assert.Equal(t, ecs.ResourceTypeGpu, aws.StringValue(back.ResourceRequirements[0].Type))
	assert.Equal(t, "2", aws.StringValue(back.ResourceRequirements[0].Value))

	ac := defs[0].toAssignContainer()
	assert.Equal(t, 2, ac.GPU)
}

// TestContainerDefsFromAWS_NoGPU_Regression covers the non-GPU path: no
// resourceRequirements means GPU stays 0 and toAWS omits the field.
func TestContainerDefsFromAWS_NoGPU_Regression(t *testing.T) {
	in := []*ecs.ContainerDefinition{{Name: aws.String("web"), Image: aws.String("registry/web:1"), Essential: aws.Bool(true)}}
	defs := containerDefsFromAWS(in)
	require.Len(t, defs, 1)
	assert.Zero(t, defs[0].GPU)
	assert.Empty(t, defs[0].toAWS().ResourceRequirements)
	assert.Zero(t, defs[0].toAssignContainer().GPU)
}

// TestGPUCountFromResourceRequirements covers the extraction edge cases: nil
// entries, non-GPU types, multiple GPU entries summed, and an invalid value
// (non-numeric or negative) skipped rather than erroring.
func TestGPUCountFromResourceRequirements(t *testing.T) {
	assert.Zero(t, gpuCountFromResourceRequirements(nil))
	assert.Zero(t, gpuCountFromResourceRequirements([]*ecs.ResourceRequirement{nil}))
	assert.Zero(t, gpuCountFromResourceRequirements([]*ecs.ResourceRequirement{
		{Type: aws.String(ecs.ResourceTypeInferenceAccelerator), Value: aws.String("device1")},
	}))
	assert.Equal(t, 3, gpuCountFromResourceRequirements([]*ecs.ResourceRequirement{
		{Type: aws.String(ecs.ResourceTypeGpu), Value: aws.String("1")},
		{Type: aws.String(ecs.ResourceTypeGpu), Value: aws.String("2")},
	}))
	assert.Zero(t, gpuCountFromResourceRequirements([]*ecs.ResourceRequirement{
		{Type: aws.String(ecs.ResourceTypeGpu), Value: aws.String("not-a-number")},
	}))
	assert.Zero(t, gpuCountFromResourceRequirements([]*ecs.ResourceRequirement{
		{Type: aws.String(ecs.ResourceTypeGpu), Value: aws.String("-1")},
	}))
}

// TestTaskDefReservedGPU covers the task-level GPU aggregate (sum across
// container defs), mirroring TestTaskDefReservedSums for CPU/memory.
func TestTaskDefReservedGPU(t *testing.T) {
	td := &TaskDefRecord{Containers: []ContainerDef{{GPU: 1}, {GPU: 3}}}
	assert.Equal(t, 4, td.reservedGPU())

	empty := &TaskDefRecord{Containers: []ContainerDef{{CPU: 128}}}
	assert.Zero(t, empty.reservedGPU())
}
