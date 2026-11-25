package vpcendpoint

import (
	"context"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memTagStore is an in-memory central tag index keyed account -> resource;
// DeleteAllTags fails while failDelete is set.
type memTagStore struct {
	mu         sync.Mutex
	tags       map[string]map[string]map[string]string
	failDelete bool
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
	if m.failDelete {
		return errInjected
	}
	delete(m.tags[account], id)
	return nil
}

func (m *memTagStore) setFailDelete(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failDelete = fail
}

func (m *memTagStore) get(account, id string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tags[account][id]
}

// DescribeTags reads the central index, so create-time tags reach it and a
// delete clears them. Absence of a record is no evidence of a delete, so the
// sweep leaves tags on an ID with no record.
func TestCentralTags_FollowTheEndpoint(t *testing.T) {
	t.Parallel()
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
	f.clock.advance(claimGrace)
	f.sweep(t)
	assert.NotNil(t, central.get(testAccount, "vpce-0gone0000000000001"), "the sweep never removes tags on an ID with no record")
	assert.NotNil(t, central.get(testAccount, live), "the sweep keeps a live endpoint's tags")
}

// A failed tag drop keeps the deleting record, the evidence the sweep needs,
// and the next pass drops the tags before it removes the record.
// Verifies: ADR-0003:S2.
func TestCentralTags_FailedDropIsRetriedBySweep(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	central := &memTagStore{tags: map[string]map[string]map[string]string{}}
	f.svc.SetCentralTagStore(central)
	id := f.create(t, rtbA1)
	require.NoError(t, central.PutResourceTags(t.Context(), testAccount, id, map[string]string{"k": "v"}))

	central.setFailDelete(true)
	out := f.del(t, id)
	assert.Empty(t, out.Unsuccessful, "the fenced endpoint is deleted for the caller")
	_, err := f.svc.DescribeVpcEndpoints(t.Context(), &ec2.DescribeVpcEndpointsInput{VpcEndpointIds: aws.StringSlice([]string{id})}, testAccount)
	requireCode(t, err, awserrors.ErrorInvalidVpcEndpointIdNotFound, "")
	rec, _, err := f.svc.load(t.Context(), testAccount, id)
	require.NoError(t, err)
	require.NotNil(t, rec, "ADR-0003:S2 the deleting record stays while its tags remain")
	assert.Equal(t, StateDeleting, rec.State)

	_, err = f.svc.Sweep(t.Context())
	require.Error(t, err, "a pass that cannot drop the tags reports it")
	central.setFailDelete(false)
	assert.Equal(t, SweepResult{Records: 1}, f.sweep(t))
	assert.Nil(t, central.get(testAccount, id))
	rec, _, err = f.svc.load(t.Context(), testAccount, id)
	require.NoError(t, err)
	assert.Nil(t, rec, "the sweep removes the record once the tags are gone")
}
