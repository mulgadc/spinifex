package handlers_ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseResourceARN(t *testing.T) {
	cases := []struct {
		name        string
		arn         string
		wantKind    ecsResourceKind
		wantCluster string
		wantID      string
	}{
		{"cluster", "arn:aws:ecs:ap-southeast-2:123456789012:cluster/web", ecsResourceCluster, "web", "web"},
		{"task-definition", "arn:aws:ecs:ap-southeast-2:123456789012:task-definition/nginx:3", ecsResourceTaskDefinition, "", "nginx:3"},
		{"service", "arn:aws:ecs:ap-southeast-2:123456789012:service/web/api", ecsResourceService, "web", "api"},
		{"task", "arn:aws:ecs:ap-southeast-2:123456789012:task/web/t-1", ecsResourceTask, "web", "t-1"},
		{"container-instance", "arn:aws:ecs:ap-southeast-2:123456789012:container-instance/web/i-1", ecsResourceContainerInstance, "web", "i-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, cluster, id, err := parseResourceARN(tc.arn)
			require.NoError(t, err)
			assert.Equal(t, tc.wantKind, kind)
			assert.Equal(t, tc.wantCluster, cluster)
			assert.Equal(t, tc.wantID, id)
		})
	}
}

func TestParseResourceARN_Invalid(t *testing.T) {
	cases := []string{
		"",
		"not-an-arn",
		"arn:aws:ecs:r:a:cluster",         // no "/" after resource type
		"arn:aws:ecs:r:a:service/onlyone", // service missing embedded name
		"arn:aws:ecs:r:a:unknown/x",       // unrecognized resource type
	}
	for _, arnStr := range cases {
		t.Run(arnStr, func(t *testing.T) {
			_, _, _, err := parseResourceARN(arnStr)
			assert.EqualError(t, err, "InvalidParameterException")
		})
	}
}

// TestService_Tags_ClusterRoundTrip covers the flat ARN shape end to end via
// the wired TagResource/ListTagsForResource/UntagResource actions.
func TestService_Tags_ClusterRoundTrip(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	created, err := svc.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	arn := aws.StringValue(created.Cluster.ClusterArn)

	_, err = svc.TagResource(ctx, &ecs.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags: []*ecs.Tag{
			{Key: aws.String("team"), Value: aws.String("infra")},
			{Key: aws.String("env"), Value: aws.String("prod")},
		},
	}, testAccountID)
	require.NoError(t, err)

	listed, err := svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.Tags, 2)

	// Merge in an extra tag alongside the existing pair.
	_, err = svc.TagResource(ctx, &ecs.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags:        []*ecs.Tag{{Key: aws.String("owner"), Value: aws.String("platform")}},
	}, testAccountID)
	require.NoError(t, err)

	listed, err = svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.Tags, 3)

	_, err = svc.UntagResource(ctx, &ecs.UntagResourceInput{
		ResourceArn: aws.String(arn),
		TagKeys:     []*string{aws.String("env")},
	}, testAccountID)
	require.NoError(t, err)

	listed, err = svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.Tags, 2)
	for _, tag := range listed.Tags {
		assert.NotEqual(t, "env", aws.StringValue(tag.Key))
	}
}

// TestService_Tags_ServiceRoundTrip covers the cluster-embedded ARN shape.
func TestService_Tags_ServiceRoundTrip(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "nginx", 128, 256)

	created, err := svc.CreateService(ctx, &ecs.CreateServiceInput{
		Cluster:        aws.String("web"),
		ServiceName:    aws.String("api"),
		TaskDefinition: aws.String("nginx"),
		DesiredCount:   aws.Int64(0),
	}, testAccountID)
	require.NoError(t, err)
	arn := aws.StringValue(created.Service.ServiceArn)

	_, err = svc.TagResource(ctx, &ecs.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags:        []*ecs.Tag{{Key: aws.String("team"), Value: aws.String("infra")}},
	}, testAccountID)
	require.NoError(t, err)

	listed, err := svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.Tags, 1)
	assert.Equal(t, "team", aws.StringValue(listed.Tags[0].Key))

	_, err = svc.UntagResource(ctx, &ecs.UntagResourceInput{
		ResourceArn: aws.String(arn),
		TagKeys:     []*string{aws.String("team")},
	}, testAccountID)
	require.NoError(t, err)

	listed, err = svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, listed.Tags)
}

// TestService_Tags_TaskDefinitionRoundTrip covers the flat family:rev ARN shape.
func TestService_Tags_TaskDefinitionRoundTrip(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	out := registerTaskDef(t, svc, "nginx", 128, 256)
	arn := aws.StringValue(out.TaskDefinition.TaskDefinitionArn)

	_, err := svc.TagResource(ctx, &ecs.TagResourceInput{
		ResourceArn: aws.String(arn),
		Tags:        []*ecs.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccountID)
	require.NoError(t, err)

	listed, err := svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.Tags, 1)

	described, err := svc.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, described.Tags, 1)
	assert.Equal(t, "env", aws.StringValue(described.Tags[0].Key))
}

// TestService_Tags_MissingResource asserts the not-found mapping the plan
// specifies: cluster/service resolve to their named NotFound errors; task and
// container-instance resolve to the generic InvalidParameter.
func TestService_Tags_MissingResource(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{
		ResourceArn: aws.String("arn:aws:ecs:ap-southeast-2:123456789012:cluster/nope"),
	}, testAccountID)
	assert.EqualError(t, err, "ClusterNotFoundException")

	_, err = svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{
		ResourceArn: aws.String("arn:aws:ecs:ap-southeast-2:123456789012:service/web/nope"),
	}, testAccountID)
	assert.EqualError(t, err, "ServiceNotFoundException")

	_, err = svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{
		ResourceArn: aws.String("arn:aws:ecs:ap-southeast-2:123456789012:task/web/nope"),
	}, testAccountID)
	assert.EqualError(t, err, "InvalidParameterException")

	_, err = svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{
		ResourceArn: aws.String("arn:aws:ecs:ap-southeast-2:123456789012:container-instance/web/nope"),
	}, testAccountID)
	assert.EqualError(t, err, "InvalidParameterException")
}

// TestService_RunTask_TagsPersisted confirms RunTask plumbs input.Tags onto
// the persisted task record and DescribeTasks reflects them back.
func TestService_RunTask_TagsPersisted(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "nginx", 128, 256)
	_, err = svc.RegisterContainerInstance(ctx, &ecs.RegisterContainerInstanceInput{
		Cluster: aws.String("web"),
		TotalResources: []*ecs.Resource{
			{Name: aws.String("CPU"), IntegerValue: aws.Int64(1024)},
			{Name: aws.String("MEMORY"), IntegerValue: aws.Int64(2048)},
		},
	}, testAccountID)
	require.NoError(t, err)

	out, err := svc.RunTask(ctx, &ecs.RunTaskInput{
		Cluster:        aws.String("web"),
		TaskDefinition: aws.String("nginx"),
		Tags:           []*ecs.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, out.Tasks, 1)
	require.Len(t, out.Tasks[0].Tags, 1)
	assert.Equal(t, "env", aws.StringValue(out.Tasks[0].Tags[0].Key))

	described, err := svc.DescribeTasks(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String("web"),
		Tasks:   []*string{out.Tasks[0].TaskArn},
	}, testAccountID)
	require.NoError(t, err)
	require.Len(t, described.Tasks, 1)
	require.Len(t, described.Tasks[0].Tags, 1)
}
