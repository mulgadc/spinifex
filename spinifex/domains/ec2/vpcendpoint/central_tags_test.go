package vpcendpoint

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memTagStore is an in-memory central tag index keyed account -> resource.
type memTagStore struct {
	mu   sync.Mutex
	tags map[string]map[string]map[string]string
}

var _ CentralTagStore = (*memTagStore)(nil)

func (m *memTagStore) PutResourceTags(_ context.Context, account, id string, tags map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tags[account] == nil {
		m.tags[account] = map[string]map[string]string{}
	}
	m.tags[account][id] = tags
	return nil
}

func (m *memTagStore) DeleteAllTags(_ context.Context, account, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tags[account], id)
	return nil
}

func (m *memTagStore) ScanTaggedResources(_ context.Context, prefix string) (map[string][]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]string{}
	for account, ids := range m.tags {
		for id := range ids {
			if strings.HasPrefix(id, prefix) {
				out[account] = append(out[account], id)
			}
		}
	}
	return out, nil
}

func (m *memTagStore) get(account, id string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tags[account][id]
}

// DescribeTags reads the central index, so create-time tags reach it and a
// delete clears them; the sweep clears tags of an endpoint that is gone.
func TestCentralTags_FollowTheEndpoint(t *testing.T) {
	f := newFixture(t)
	central := &memTagStore{tags: map[string]map[string]map[string]string{}}
	f.svc.SetCentralTagStore(central)

	e, err := f.svc.CreateVpcEndpoint(t.Context(), &ec2.CreateVpcEndpointInput{
		VpcId: aws.String(vpcA), ServiceName: aws.String(s3Service),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String(ec2.ResourceTypeVpcEndpoint),
			Tags:         []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("s3")}},
		}},
	}, testAccount)
	require.NoError(t, err)
	id := aws.StringValue(e.VpcEndpoint.VpcEndpointId)
	assert.Equal(t, map[string]string{"Name": "s3"}, central.get(testAccount, id))

	f.del(t, id)
	assert.Nil(t, central.get(testAccount, id), "delete clears the endpoint's central tags")

	require.NoError(t, central.PutResourceTags(t.Context(), testAccount, "vpce-0gone0000000000001", map[string]string{"k": "v"}))
	live := f.create(t)
	require.NoError(t, central.PutResourceTags(t.Context(), testAccount, live, map[string]string{"k": "v"}))
	f.sweep(t)
	assert.Nil(t, central.get(testAccount, "vpce-0gone0000000000001"), "the sweep clears tags of an endpoint with no record")
	assert.NotNil(t, central.get(testAccount, live), "the sweep keeps a live endpoint's tags")
}
