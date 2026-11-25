package tgw

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCentralTags struct {
	mu      sync.Mutex
	tags    map[string]map[string]string // resource ID to tags
	owner   map[string]string            // resource ID to account
	deleted []string
}

var _ CentralTagStore = (*fakeCentralTags)(nil)

func (f *fakeCentralTags) PutResourceTags(_ context.Context, accountID, id string, tags map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags[id], f.owner[id] = tags, accountID
	return nil
}

func (f *fakeCentralTags) DeleteAllTags(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tags, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeCentralTags) ScanTaggedResources(_ context.Context, prefix string) (map[string][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]string{}
	for id := range f.tags {
		if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
			out[f.owner[id]] = append(out[f.owner[id]], id)
		}
	}
	return out, nil
}

func TestRecordTags_MirrorAndSweep(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	central := &fakeCentralTags{tags: map[string]map[string]string{}, owner: map[string]string{}}
	f.svc.SetCentralTagStore(central)
	out, err := f.svc.CreateTransitGateway(t.Context(), &ec2.CreateTransitGatewayInput{
		TagSpecifications: []*ec2.TagSpecification{{ResourceType: aws.String(ec2.ResourceTypeTransitGateway),
			Tags: []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("hub")}}}},
	}, testAccount)
	require.NoError(t, err)
	id := aws.StringValue(out.TransitGateway.TransitGatewayId)
	assert.Equal(t, map[string]string{"Name": "hub"}, central.tags[id], "create projects its tags")

	require.NoError(t, f.svc.ApplyRecordTags(&ec2.CreateTagsInput{
		Resources: aws.StringSlice([]string{id}), Tags: []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}},
	}, testAccount))
	require.NoError(t, f.svc.RemoveRecordTags(&ec2.DeleteTagsInput{
		Resources: aws.StringSlice([]string{id}), Tags: []*ec2.Tag{{Key: aws.String("Name")}},
	}, testAccount))
	filtered, err := f.svc.DescribeTransitGateways(t.Context(), &ec2.DescribeTransitGatewaysInput{
		Filters: []*ec2.Filter{{Name: aws.String("tag:env"), Values: aws.StringSlice([]string{"prod"})}},
	}, testAccount)
	require.NoError(t, err)
	require.Len(t, filtered.TransitGateways, 1)
	assert.Equal(t, []*ec2.Tag{{Key: aws.String("env"), Value: aws.String("prod")}}, filtered.TransitGateways[0].Tags)

	// A tag entry for a resource in neither bucket is removed; a live one stays.
	central.tags["tgw-0123456789abcdef0"], central.owner["tgw-0123456789abcdef0"] = map[string]string{"x": "y"}, testAccount
	res, err := f.svc.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, res.TagsRemoved)
	assert.Equal(t, []string{"tgw-0123456789abcdef0"}, central.deleted)
}
