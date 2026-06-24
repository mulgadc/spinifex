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
