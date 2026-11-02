package ecs

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_Tagging_ tests pin how the ECS tag actions read and
// write resource records today. They describe current behaviour, not the
// target design, and a failure means the behaviour changed.

// interleavingKV runs between once, after the first read of key returns and
// before the caller can write, standing in for a concurrent writer.
type interleavingKV struct {
	jetstream.KeyValue

	key     string
	once    sync.Once
	between func()
}

func (k *interleavingKV) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	entry, err := k.KeyValue.Get(ctx, key)
	if key == k.key {
		k.once.Do(k.between)
	}
	return entry, err
}

// A tag write re-puts the whole record it read without a revision check, so a
// record change committed between its read and its write is reverted.
func TestCurrentBehaviour_Tagging_ECSTagWriteRevertsInterleavedRecordWrite(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	registerTaskDef(t, svc, "nginx", 128, 256)
	_, err = svc.CreateService(ctx, &ecs.CreateServiceInput{
		Cluster:        aws.String("web"),
		ServiceName:    aws.String("api"),
		TaskDefinition: aws.String("nginx"),
		DesiredCount:   aws.Int64(0),
	}, testAccountID)
	require.NoError(t, err)

	kv, err := svc.bucket(ctx, testAccountID)
	require.NoError(t, err)
	key := ServiceKey("web", "api")
	wrapped := &interleavingKV{KeyValue: kv, key: key, between: func() {
		var rec ServiceRecord
		found, gerr := getJSON(ctx, kv, key, &rec)
		require.NoError(t, gerr)
		require.True(t, found)
		rec.DesiredCount = 5
		require.NoError(t, putJSON(ctx, kv, key, &rec))
	}}

	err = svc.mutateResourceTags(ctx, wrapped, ecsResourceService, "web", "api", func(existing map[string]string) map[string]string {
		if existing == nil {
			existing = map[string]string{}
		}
		existing["team"] = "infra"
		return existing
	})
	require.NoError(t, err)

	var final ServiceRecord
	found, err := getJSON(ctx, kv, key, &final)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 0, final.DesiredCount, "the interleaved DesiredCount=5 write is lost")
	assert.Equal(t, map[string]string{"team": "infra"}, final.Tags)
}

// The ARN's account and region are not compared with the caller's: the
// resource segment is resolved in the caller's own bucket, so a foreign ARN
// tags the caller's same-named cluster.
func TestCurrentBehaviour_Tagging_ECSForeignAccountARNTagsCallerResource(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	_, err := svc.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)

	foreign := "arn:aws:ecs:eu-west-9:999999999999:cluster/web"
	_, err = svc.TagResource(ctx, &ecs.TagResourceInput{
		ResourceArn: aws.String(foreign),
		Tags:        []*ecs.Tag{{Key: aws.String("via"), Value: aws.String("foreign-arn")}},
	}, testAccountID)
	require.NoError(t, err)

	own := "arn:aws:ecs:" + testRegion + ":" + testAccountID + ":cluster/web"
	listed, err := svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{ResourceArn: aws.String(own)}, testAccountID)
	require.NoError(t, err)
	require.Len(t, listed.Tags, 1)
	assert.Equal(t, "foreign-arn", aws.StringValue(listed.Tags[0].Value))
}

// TagResource applies no tag-count, key-length, value-length or aws: prefix
// rule: all 52 tags below are stored.
func TestCurrentBehaviour_Tagging_ECSTagResourceAppliesNoTagLimits(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	created, err := svc.CreateCluster(ctx, &ecs.CreateClusterInput{ClusterName: aws.String("web")}, testAccountID)
	require.NoError(t, err)
	arn := aws.StringValue(created.Cluster.ClusterArn)

	tags := []*ecs.Tag{
		{Key: aws.String("aws:reserved"), Value: aws.String("v")},
		{Key: aws.String(strings.Repeat("k", 129)), Value: aws.String(strings.Repeat("v", 257))},
	}
	for i := range 50 {
		tags = append(tags, &ecs.Tag{Key: aws.String("k" + strings.Repeat("x", i+1)), Value: aws.String("v")})
	}
	_, err = svc.TagResource(ctx, &ecs.TagResourceInput{ResourceArn: aws.String(arn), Tags: tags}, testAccountID)
	require.NoError(t, err)

	listed, err := svc.ListTagsForResource(ctx, &ecs.ListTagsForResourceInput{ResourceArn: aws.String(arn)}, testAccountID)
	require.NoError(t, err)
	assert.Len(t, listed.Tags, 52)
}
